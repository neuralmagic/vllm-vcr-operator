package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpSucceedsAndDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(--help) = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "--device-count") {
		t.Fatalf("--help output missing flags:\n%s", stdout.String())
	}
}

func TestRunReportsUnusableSocketDirectory(t *testing.T) {
	root := t.TempDir()
	socketDir := filepath.Join(root, "socket-dir")
	if err := os.WriteFile(socketDir, nil, 0o600); err != nil {
		t.Fatalf("prepare socket dir placeholder: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"--socket-dir", socketDir,
		"--device-count-file", filepath.Join(root, "device-count"),
		"--kubelet-socket", filepath.Join(root, "kubelet.sock"),
		"--health-port", "0",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run succeeded with an unusable socket directory, want error")
	}
	if !strings.Contains(err.Error(), socketDir) {
		t.Fatalf("run error = %v, want it to name %q", err, socketDir)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--not-a-flag"}, &stdout, &stderr); err == nil {
		t.Fatal("run accepted an unknown flag, want error")
	}
}
