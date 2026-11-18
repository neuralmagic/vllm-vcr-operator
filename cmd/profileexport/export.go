package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// stagingPrefix names the temporary directory used to stage an export inside the
// target directory. Staging must live in the target because that is the only path
// the provisioner can write to (the operator runs it with a read-only root
// filesystem and only the profile volume writable), and because publishing is then
// a rename inside one directory.
const stagingPrefix = ".vcr-profile-export-"

// summary is the single-line JSON document printed on success.
type summary struct {
	ProfileID     string `json:"profileId"`
	MetadataFile  string `json:"metadataFile"`
	TraceFile     string `json:"traceFile"`
	MetadataBytes int64  `json:"metadataBytes"`
	TraceBytes    int64  `json:"traceBytes"`
	Source        string `json:"source"`
	Target        string `json:"target"`
}

// export validates the profile image filesystem in source and replaces target
// with an exported copy of its metadata and trace documents.
//
// Nothing is written to target unless every document has been validated and
// fully staged, so a failure leaves no partially populated profile volume.
func export(ctx context.Context, source, target string) (summary, error) {
	if err := ctx.Err(); err != nil {
		return summary{}, err
	}

	metadataPath := filepath.Join(source, names.ProfileMetadataFileName)
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return summary{}, fmt.Errorf("source %q: read profile metadata: %w", source, err)
	}
	metadata, err := profile.Parse(raw)
	if err != nil {
		return summary{}, fmt.Errorf("source %q: invalid %s: %w", source, names.ProfileMetadataFileName, err)
	}

	exportedMetadataName, err := safeFileName("provisioning.metadataFile", metadata.Provisioning.MetadataFile)
	if err != nil {
		return summary{}, fmt.Errorf("source %q: %w", source, err)
	}
	if exportedMetadataName != names.ProfileMetadataFileName {
		return summary{}, fmt.Errorf(
			"source %q: provisioning.metadataFile %q conflicts with the profile volume file name %q",
			source, exportedMetadataName, names.ProfileMetadataFileName)
	}

	traceName, err := safeFileName("trace.file", metadata.Trace.File)
	if err != nil {
		return summary{}, fmt.Errorf("source %q: %w", source, err)
	}
	tracePath := filepath.Join(source, traceName)
	traceInfo, err := os.Stat(tracePath)
	if err != nil {
		return summary{}, fmt.Errorf("source %q: read trace file %q declared by metadata.trace.file: %w", source, traceName, err)
	}
	if !traceInfo.Mode().IsRegular() {
		return summary{}, fmt.Errorf("source %q: trace file %q is not a regular file", source, traceName)
	}
	if traceInfo.Size() == 0 {
		return summary{}, fmt.Errorf("source %q: trace file %q is empty: a profile without recorded data cannot be served", source, traceName)
	}

	// The provisioner's root filesystem is read-only and the profile volume is
	// mounted at target, so nothing may be written beside or above it.
	if err := os.MkdirAll(target, 0o755); err != nil {
		return summary{}, fmt.Errorf("prepare target directory %q: %w", target, err)
	}
	staging, err := os.MkdirTemp(target, stagingPrefix)
	if err != nil {
		return summary{}, fmt.Errorf("create staging directory in %q: %w", target, err)
	}
	defer func() {
		// Every path removes the staging directory, including the error paths: a
		// leftover staging directory is debris that the next export has to clean up.
		// A workspace that vanished mid-export is not an error worth reporting twice.
		_ = removeAll(staging)
	}()

	// Stage the trace first and publish it first: metadata.json is the document
	// that makes a profile volume complete, so it appears last and never before the
	// trace it declares is in place.
	traceBytes, err := copyFile(tracePath, filepath.Join(staging, traceName))
	if err != nil {
		return summary{}, fmt.Errorf("stage trace file %q: %w", traceName, err)
	}
	if err := writeFile(filepath.Join(staging, names.ProfileMetadataFileName), raw); err != nil {
		return summary{}, fmt.Errorf("stage profile metadata: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return summary{}, err
	}
	// Replace, never merge: a stale profile must not survive next to the
	// freshly exported documents.
	if err := clearTarget(target, staging); err != nil {
		return summary{}, err
	}
	if err := os.Rename(filepath.Join(staging, traceName), filepath.Join(target, traceName)); err != nil {
		return summary{}, fmt.Errorf("publish trace file %q into %q: %w", traceName, target, err)
	}
	if err := os.Rename(filepath.Join(staging, names.ProfileMetadataFileName),
		filepath.Join(target, names.ProfileMetadataFileName)); err != nil {
		return summary{}, fmt.Errorf("publish profile metadata into %q: %w", target, err)
	}
	if err := os.Remove(staging); err != nil {
		return summary{}, fmt.Errorf("remove staging directory %q: %w", staging, err)
	}

	return summary{
		ProfileID:     metadata.ID,
		MetadataFile:  names.ProfileMetadataFileName,
		TraceFile:     traceName,
		MetadataBytes: int64(len(raw)),
		TraceBytes:    traceBytes,
		Source:        source,
		Target:        target,
	}, nil
}

// clearTarget removes every entry of the target directory except the staging
// directory, so a profile volume that already holds an export is replaced rather
// than merged. Everything the export publishes is moved in afterwards.
func clearTarget(target, staging string) error {
	entries, err := os.ReadDir(target)
	if err != nil {
		return fmt.Errorf("read target directory %q: %w", target, err)
	}
	for _, entry := range entries {
		path := filepath.Join(target, entry.Name())
		if path == staging {
			continue
		}
		if err := removeAll(path); err != nil {
			return fmt.Errorf("remove stale entry %q from the target directory: %w", path, err)
		}
	}
	return nil
}

// removeAll removes path, including content that lost write permission. An
// interrupted export leaves a staging directory behind, and the provisioner runs
// as an unprivileged user, so a plain RemoveAll can fail with "permission denied"
// on a directory it still owns.
func removeAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.IsDir() {
		return err
	}
	// Best effort: make every directory traversable and writable, then retry. A
	// path this fails on is reported by the retry below.
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return os.Chmod(current, 0o600)
	})
	return os.RemoveAll(path)
}

// safeFileName rejects document names that could escape the profile directory.
// The name comes from image-provided metadata, so it is treated as untrusted.
func safeFileName(setting, name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%s is empty", setting)
	}
	if trimmed == "." || trimmed == ".." || filepath.IsAbs(trimmed) ||
		strings.ContainsRune(trimmed, os.PathSeparator) ||
		strings.ContainsAny(trimmed, `/\`) ||
		filepath.Base(trimmed) != trimmed {
		return "", fmt.Errorf("%s %q must be a plain file name inside the profile image directory", setting, name)
	}
	return trimmed, nil
}

// writeFile writes data to path durably.
func writeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// copyFile copies source to destination durably and returns the byte count.
func copyFile(source, destination string) (int64, error) {
	from, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer func() { _ = from.Close() }()

	to, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(to, from)
	if err != nil {
		_ = to.Close()
		return written, err
	}
	if err := to.Sync(); err != nil {
		_ = to.Close()
		return written, err
	}
	return written, to.Close()
}
