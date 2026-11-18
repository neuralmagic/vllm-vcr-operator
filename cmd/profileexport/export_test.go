package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validMetadata is the contract example from docs/contracts/profile-image.md.
const validMetadata = `{
  "schemaVersion": "vcr.opendatahub-io/profile/v1",
  "id": "qwen3-0.6b-h200-tp1-bf16",
  "model": {
    "name": "Qwen/Qwen3-0.6B",
    "revision": "c9340e6f3",
    "tokenizer": "Qwen/Qwen3-0.6B",
    "tokenizerRevision": "c9340e6f3",
    "chatTemplateHash": "sha256:abc123"
  },
  "gpu": { "type": "H200", "count": 1 },
  "execution": {
    "tensorParallel": 1,
    "pipelineParallel": 1,
    "dtype": "bfloat16",
    "quantization": ""
  },
  "cache": {
    "blockSize": 64,
    "enablePrefixCaching": true,
    "kvBlocks": 2048,
    "chunkedPrefill": true
  },
  "vllm": { "version": "0.33.0", "configHash": "sha256:def456" },
  "coverage": {
    "maxModelLen": { "min": 1, "max": 131072 },
    "maxNumSeqs": { "min": 1, "max": 128 },
    "maxNumBatchedTokens": { "min": 1, "max": 8192 },
    "longPrefillTokenThreshold": { "min": 0, "max": 4096 },
    "kvBlocks": { "min": 1, "max": 2048 },
    "schedulingPolicies": ["fcfs", "priority"],
    "kvMemoryConversion": false
  },
  "trace": {
    "file": "trace.jsonl.gz",
    "format": "vllm-vcr/jsonl",
    "recordedTokens": true,
    "latencyModel": true,
    "kvEvents": true
  },
  "provisioning": {
    "entrypoint": ["/usr/local/bin/vcr-profile-export"],
    "metadataFile": "metadata.json",
    "traceFile": "trace.jsonl.gz"
  },
  "capabilities": {
    "chatCompletions": true,
    "completions": true,
    "messages": false,
    "responses": false,
    "streaming": true,
    "toolCallParsers": [],
    "reasoningParsers": [],
    "kvEventReplay": false,
    "metrics": ["vllm:num_requests_running", "vllm:kv_cache_usage_perc"]
  }
}
`

const validTrace = "{\"kind\":\"request\"}\n{\"kind\":\"response\"}\n"

// writeProfileSource creates a profile image filesystem containing the supplied
// documents and returns its path.
func writeProfileSource(t *testing.T, metadata string, trace []byte) string {
	t.Helper()
	source := t.TempDir()
	if metadata != "" {
		if err := os.WriteFile(filepath.Join(source, "metadata.json"), []byte(metadata), 0o644); err != nil {
			t.Fatalf("write metadata: %v", err)
		}
	}
	if trace != nil {
		if err := os.WriteFile(filepath.Join(source, "trace.jsonl.gz"), trace, 0o644); err != nil {
			t.Fatalf("write trace: %v", err)
		}
	}
	return source
}

// runExport invokes the command entrypoint and returns its exit code and
// captured streams.
func runExport(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// decodeSummary parses the single-line JSON summary printed on success.
func decodeSummary(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want a single-line summary: %q", len(lines), stdout)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &summary); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	return summary
}

// targetEntries lists the entries of dir, treating a missing dir as empty.
func targetEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read target %q: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestExportCopiesMetadataAndTrace(t *testing.T) {
	source := writeProfileSource(t, validMetadata, []byte(validTrace))
	target := filepath.Join(t.TempDir(), "profile")

	code, stdout, stderr := runExport(t, "--source", source, target)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	wantFiles := map[string]string{
		"metadata.json":  validMetadata,
		"trace.jsonl.gz": validTrace,
	}
	for name, want := range wantFiles {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("read exported %q: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("exported %q = %q, want byte-identical %q", name, got, want)
		}
	}
	if entries := targetEntries(t, target); len(entries) != len(wantFiles) {
		t.Errorf("target contains %v, want exactly %v", entries, wantFiles)
	}

	summary := decodeSummary(t, stdout)
	if got := summary["profileId"]; got != "qwen3-0.6b-h200-tp1-bf16" {
		t.Errorf("summary profileId = %v, want the metadata id", got)
	}
	if got := summary["traceFile"]; got != "trace.jsonl.gz" {
		t.Errorf("summary traceFile = %v, want trace.jsonl.gz", got)
	}
	if got := summary["metadataBytes"]; got != float64(len(validMetadata)) {
		t.Errorf("summary metadataBytes = %v, want %d", got, len(validMetadata))
	}
	if got := summary["traceBytes"]; got != float64(len(validTrace)) {
		t.Errorf("summary traceBytes = %v, want %d", got, len(validTrace))
	}
}

func TestExportFailsWhenTraceMissing(t *testing.T) {
	source := writeProfileSource(t, validMetadata, nil)
	target := filepath.Join(t.TempDir(), "profile")

	code, stdout, stderr := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (stdout: %s)", stdout)
	}
	if !strings.Contains(stderr, "trace.jsonl.gz") {
		t.Errorf("stderr = %q, want it to name the missing trace file", stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}

func TestExportRejectsInvalidMetadata(t *testing.T) {
	invalid := strings.Replace(validMetadata, `"vcr.opendatahub-io/profile/v1"`, `"vcr.opendatahub-io/profile/v2"`, 1)
	source := writeProfileSource(t, invalid, []byte(validTrace))
	target := filepath.Join(t.TempDir(), "profile")

	code, stdout, stderr := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (stdout: %s)", stdout)
	}
	if !strings.Contains(stderr, "metadata.json") {
		t.Errorf("stderr = %q, want it to name metadata.json", stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}

func TestExportRejectsEmptyTrace(t *testing.T) {
	source := writeProfileSource(t, validMetadata, []byte{})
	target := filepath.Join(t.TempDir(), "profile")

	code, _, stderr := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for an empty trace file")
	}
	if !strings.Contains(stderr, "trace.jsonl.gz") {
		t.Errorf("stderr = %q, want it to name the empty trace file", stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}

func TestExportReplacesStaleTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create stale target: %v", err)
	}
	stale := map[string]string{
		"metadata.json":  `{"schemaVersion":"stale"}`,
		"trace.jsonl.gz": "stale trace",
		"left-over-file": "stale artifact",
	}
	for name, content := range stale {
		if err := os.WriteFile(filepath.Join(target, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write stale %q: %v", name, err)
		}
	}
	source := writeProfileSource(t, validMetadata, []byte(validTrace))

	code, _, stderr := runExport(t, "--source", source, target)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 2 {
		t.Errorf("target contains %v, want only the freshly exported files", entries)
	}
	got, err := os.ReadFile(filepath.Join(target, "metadata.json"))
	if err != nil {
		t.Fatalf("read exported metadata: %v", err)
	}
	if string(got) != validMetadata {
		t.Errorf("exported metadata = %q, want the fresh document", got)
	}
}

func TestExportIsIdempotent(t *testing.T) {
	source := writeProfileSource(t, validMetadata, []byte(validTrace))
	parent := t.TempDir()
	target := filepath.Join(parent, "profile")

	firstCode, firstStdout, firstStderr := runExport(t, "--source", source, target)
	if firstCode != 0 {
		t.Fatalf("first run exit code = %d (stderr: %s)", firstCode, firstStderr)
	}
	secondCode, secondStdout, secondStderr := runExport(t, "--source", source, target)
	if secondCode != 0 {
		t.Fatalf("second run exit code = %d (stderr: %s)", secondCode, secondStderr)
	}
	if firstStdout != secondStdout {
		t.Errorf("summary changed across runs: %q vs %q", firstStdout, secondStdout)
	}
	if entries := targetEntries(t, target); len(entries) != 2 {
		t.Errorf("target contains %v after two runs, want two files", entries)
	}
	leftovers, err := filepath.Glob(filepath.Join(parent, ".vcr-profile-export-*"))
	if err != nil {
		t.Fatalf("glob staging dirs: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("staging directories left behind: %v", leftovers)
	}
}

func TestExportRequiresTargetDirectory(t *testing.T) {
	code, _, stderr := runExport(t, "--source", t.TempDir())
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero without a target directory")
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr = %q, want usage guidance", stderr)
	}
}

func TestExportDefaultsToProfileSource(t *testing.T) {
	code, _, stderr := runExport(t, filepath.Join(t.TempDir(), "profile"))
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero when the default source is absent")
	}
	if !strings.Contains(stderr, "/profile") {
		t.Errorf("stderr = %q, want it to name the default source directory", stderr)
	}
}

func TestExportRejectsTracePathEscapingTarget(t *testing.T) {
	escaping := strings.Replace(validMetadata, `"file": "trace.jsonl.gz"`, `"file": "../escape.jsonl.gz"`, 1)
	source := writeProfileSource(t, escaping, nil)
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "escape.jsonl.gz"), []byte(validTrace), 0o644); err != nil {
		t.Fatalf("write escaping trace: %v", err)
	}
	target := filepath.Join(t.TempDir(), "profile")

	code, stdout, _ := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for a trace path escaping the image directory (stdout: %s)", stdout)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}

func TestExportRejectsSourceWithoutMetadata(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "profile")

	code, _, stderr := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero without metadata.json")
	}
	if !strings.Contains(stderr, "metadata.json") {
		t.Errorf("stderr = %q, want it to name metadata.json", stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}

func TestExportRejectsMetadataFileNameConflict(t *testing.T) {
	conflicting := strings.Replace(validMetadata, `"metadataFile": "metadata.json"`, `"metadataFile": "profile.json"`, 1)
	source := writeProfileSource(t, conflicting, []byte(validTrace))
	target := filepath.Join(t.TempDir(), "profile")

	code, stdout, stderr := runExport(t, "--source", source, target)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when provisioning.metadataFile diverges from the profile volume name (stdout: %s)", stdout)
	}
	if !strings.Contains(stderr, "provisioning.metadataFile") {
		t.Errorf("stderr = %q, want it to name provisioning.metadataFile", stderr)
	}
	if entries := targetEntries(t, target); len(entries) != 0 {
		t.Errorf("failed export left target populated with %v", entries)
	}
}
