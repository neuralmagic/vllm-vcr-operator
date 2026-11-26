package registry_test

import (
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
)

const base = "ghcr.io/acme/vllm-vcr-profiles"

func TestJoinReferenceResolvesInsideTheConfiguredRegistry(t *testing.T) {
	for _, c := range []struct{ reference, want string }{
		// With a registry configured, a profile is an identity inside it.
		{reference: "qwen3-0.6b-h200-tp1-bf16", want: base + ":qwen3-0.6b-h200-tp1-bf16"},
		{reference: "qwen3-0.6b-h200-tp1-bf16-0.1.0", want: base + ":qwen3-0.6b-h200-tp1-bf16-0.1.0"},
		{reference: "nested/qwen3-0.6b-h200-tp1-bf16", want: base + "/nested/qwen3-0.6b-h200-tp1-bf16"},
		// An absolute reference that already lives under the base is kept verbatim,
		// digest included.
		{reference: base + ":0.1.0", want: base + ":0.1.0"},
		{reference: base + "@sha256:" + strings.Repeat("a", 64), want: base + "@sha256:" + strings.Repeat("a", 64)},
	} {
		got, err := registry.JoinReference(base, c.reference)
		if err != nil {
			t.Errorf("JoinReference(%q): %v", c.reference, err)
			continue
		}
		if got != c.want {
			t.Errorf("JoinReference(%q) = %q, want %q", c.reference, got, c.want)
		}
	}
}

// TestJoinReferenceRefusesAnotherRegistry: one configured registry means one source of
// profiles. A reference to a different host is refused rather than fetched, because it
// would carry no registration at all.
func TestJoinReferenceRefusesAnotherRegistry(t *testing.T) {
	_, err := registry.JoinReference(base, "quay.io/opendatahub/vllm-vcr-profiles:qwen3-0.6b")
	if err == nil {
		t.Fatal("a profile outside the configured registry was accepted")
	}
	for _, want := range []string{"quay.io", base, "profileRegistry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestJoinReferenceWithoutABaseKeepsAbsoluteReferences: a cluster with no profile registry
// configured works as before, so existing configurations do not change meaning.
func TestJoinReferenceWithoutABaseKeepsAbsoluteReferences(t *testing.T) {
	reference := "ghcr.io/neuralmagic/vllm-vcr-profiles:qwen3-0.6b-h200-tp1-bf16"
	got, err := registry.JoinReference("", reference)
	if err != nil {
		t.Fatalf("JoinReference: %v", err)
	}
	if got != reference {
		t.Errorf("JoinReference = %q, want the reference unchanged", got)
	}
}

func TestJoinReferenceRejectsAnEmptyReference(t *testing.T) {
	if _, err := registry.JoinReference(base, "  "); err == nil {
		t.Fatal("an empty reference was accepted")
	}
}
