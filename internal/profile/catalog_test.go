package profile_test

import (
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// metadataFor returns a contract-valid profile document. Tests override the
// single field under test, so every case starts from the documented shape.
func metadataFor(id string) *profile.Metadata {
	return &profile.Metadata{
		SchemaVersion: names.ProfileCatalogSchemaVersion,
		ID:            id,
		Model: profile.Model{
			Name:              "Qwen/Qwen3-0.6B",
			Revision:          "c9340e6f3",
			Tokenizer:         "Qwen/Qwen3-0.6B",
			TokenizerRevision: "c9340e6f3",
			ChatTemplateHash:  "sha256:chat-template",
		},
		GPU: profile.GPU{Type: "H200", Count: 1},
		Execution: profile.Execution{
			TensorParallel:   1,
			PipelineParallel: 1,
			DType:            "bfloat16",
			Quantization:     "",
		},
		Cache: profile.Cache{BlockSize: 64, EnablePrefixCaching: true, KVBlocks: 2048},
		VLLM: profile.VLLM{
			Version:    names.SupportedVLLMVersion,
			ConfigHash: "sha256:config",
		},
		Coverage: profile.Coverage{
			MaxModelLen:               &profile.Range{Min: 1, Max: 131072},
			MaxNumSeqs:                &profile.Range{Min: 1, Max: 128},
			MaxNumBatchedTokens:       &profile.Range{Min: 1, Max: 8192},
			LongPrefillTokenThreshold: &profile.Range{Min: 0, Max: 4096},
			KVBlocks:                  &profile.Range{Min: 1, Max: 2048},
			SchedulingPolicies:        []string{"fcfs", "priority"},
		},
		Trace: profile.Trace{
			File:           "trace.jsonl.gz",
			Format:         profile.TraceFormatJSONL,
			RecordedTokens: true,
			LatencyModel:   true,
		},
		Provisioning: profile.Provisioning{
			Entrypoint:   []string{"/usr/local/bin/vcr-profile-export"},
			MetadataFile: names.ProfileMetadataFileName,
			TraceFile:    "trace.jsonl.gz",
		},
		Capabilities: profile.Capabilities{ChatCompletions: true, Completions: true, Streaming: true},
	}
}

// matchingRequirements are the workload settings metadataFor(id) satisfies.
func matchingRequirements() profile.Requirements {
	prefixCaching := true
	return profile.Requirements{
		Model:               "Qwen/Qwen3-0.6B",
		ModelRevision:       "c9340e6f3",
		GPUType:             "H200",
		GPUCount:            1,
		TensorParallel:      1,
		PipelineParallel:    1,
		DType:               "bfloat16",
		Quantization:        "",
		BlockSize:           64,
		EnablePrefixCaching: &prefixCaching,
	}
}

func candidate(id, image, digest string) profile.Candidate {
	return profile.Candidate{Image: image, Digest: digest, Metadata: metadataFor(id)}
}

func boolPtr(v bool) *bool { return &v }

func TestNewCatalogSelectsSingleCompatibleProfile(t *testing.T) {
	a100 := metadataFor("a100-tp1")
	a100.GPU.Type = "A100"

	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/qwen3-h200:1", "sha256:aaaa"),
		{Image: "registry.example/vcr/qwen3-a100:1", Digest: "sha256:bbbb", Metadata: a100},
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	selection, err := catalog.Select(matchingRequirements())
	if err != nil {
		t.Fatalf("Select() returned error: %v", err)
	}
	if got, want := selection.ID, "h200-tp1"; got != want {
		t.Errorf("Selection.ID = %q, want %q", got, want)
	}
	if got, want := selection.Image, "registry.example/vcr/qwen3-h200:1"; got != want {
		t.Errorf("Selection.Image = %q, want %q", got, want)
	}
	if got, want := selection.Digest, "sha256:aaaa"; got != want {
		t.Errorf("Selection.Digest = %q, want %q", got, want)
	}
	if selection.Metadata == nil || selection.Metadata.ID != "h200-tp1" {
		t.Errorf("Selection.Metadata = %+v, want metadata for h200-tp1", selection.Metadata)
	}
}

func TestSelectToleratesUnpinnedRevisionButNotAMismatch(t *testing.T) {
	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/qwen3-h200:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	req := matchingRequirements()
	req.ModelRevision = ""
	if _, err := catalog.Select(req); err != nil {
		t.Fatalf("Select() with unpinned revision returned error: %v", err)
	}

	req.ModelRevision = "deadbeef"
	_, err = catalog.Select(req)
	if err == nil {
		t.Fatal("Select() accepted a revision the profile was not recorded against, want rejection")
	}
	for _, sub := range []string{"model.revision", "deadbeef", "c9340e6f3", "h200-tp1"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestSelectRejectsIncompatibleRequirements(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(m *profile.Metadata)
		req     func(r *profile.Requirements)
		wantSub []string
	}{
		{
			name:    "model name mismatch",
			req:     func(r *profile.Requirements) { r.Model = "meta-llama/Llama-3-8B" },
			wantSub: []string{"model.name", "meta-llama/Llama-3-8B", "Qwen/Qwen3-0.6B"},
		},
		{
			name:    "gpu type mismatch",
			req:     func(r *profile.Requirements) { r.GPUType = "A100" },
			wantSub: []string{"gpu.type", "A100", "H200"},
		},
		{
			name:    "gpu count mismatch",
			req:     func(r *profile.Requirements) { r.GPUCount = 2; r.TensorParallel = 2 },
			wantSub: []string{"gpu.count", "2", "1"},
		},
		{
			name:    "dtype mismatch",
			req:     func(r *profile.Requirements) { r.DType = "float16" },
			wantSub: []string{"execution.dtype", "float16", "bfloat16"},
		},
		{
			name:    "quantized profile cannot serve unquantized workload",
			mutate:  func(m *profile.Metadata) { m.Execution.Quantization = "awq" },
			wantSub: []string{"execution.quantization", "awq", "unquantized"},
		},
		{
			name: "unquantized profile cannot serve quantized workload",
			req:  func(r *profile.Requirements) { r.Quantization = "awq" },
			wantSub: []string{
				"execution.quantization", "awq",
			},
		},
		{
			name:    "tokenizer pinned by the workload",
			mutate:  func(m *profile.Metadata) { m.Model.Tokenizer = "Qwen/Qwen3-0.6B-Base" },
			req:     func(r *profile.Requirements) { r.Tokenizer = "Qwen/Qwen3-0.6B" },
			wantSub: []string{"model.tokenizer", "Qwen/Qwen3-0.6B-Base", "Qwen/Qwen3-0.6B"},
		},
		{
			name:    "tokenizer revision pinned by the workload",
			mutate:  func(m *profile.Metadata) { m.Model.TokenizerRevision = "old" },
			req:     func(r *profile.Requirements) { r.TokenizerRevision = "c9340e6f3" },
			wantSub: []string{"model.tokenizerRevision", "old", "c9340e6f3"},
		},
		{
			name:    "chat template hash pinned by the workload",
			mutate:  func(m *profile.Metadata) { m.Model.ChatTemplateHash = "sha256:other" },
			req:     func(r *profile.Requirements) { r.ChatTemplateHash = "sha256:chat-template" },
			wantSub: []string{"model.chatTemplateHash", "sha256:other", "sha256:chat-template"},
		},
		{
			name:    "block size mismatch",
			req:     func(r *profile.Requirements) { r.BlockSize = 16 },
			wantSub: []string{"cache.blockSize", "16", "64"},
		},
		{
			name:    "prefix caching mode mismatch",
			req:     func(r *profile.Requirements) { r.EnablePrefixCaching = boolPtr(false) },
			wantSub: []string{"cache.enablePrefixCaching", "false", "true"},
		},
		{
			name:    "profile recorded against another vllm release",
			mutate:  func(m *profile.Metadata) { m.VLLM.Version = "0.23.0" },
			wantSub: []string{"vllm.version", "0.23.0", names.SupportedVLLMVersion},
		},
		{
			name:    "tensor parallel inconsistent with device count",
			mutate:  func(m *profile.Metadata) {},
			req:     func(r *profile.Requirements) { r.GPUCount = 2; r.TensorParallel = 1 },
			wantSub: []string{"execution.tensorParallel", "gpu.count", "1", "2"},
		},
		{
			name:    "pipeline parallel greater than one",
			mutate:  func(m *profile.Metadata) {},
			req:     func(r *profile.Requirements) { r.PipelineParallel = 2 },
			wantSub: []string{"execution.pipelineParallel", "2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := metadataFor("h200-tp1")
			if tc.mutate != nil {
				tc.mutate(m)
			}
			catalog, err := profile.NewCatalog([]profile.Candidate{
				{Image: "registry.example/vcr/profile:1", Digest: "sha256:aaaa", Metadata: m},
			})
			if err != nil {
				t.Fatalf("NewCatalog() returned error: %v", err)
			}

			req := matchingRequirements()
			if tc.req != nil {
				tc.req(&req)
			}

			_, err = catalog.Select(req)
			if err == nil {
				t.Fatal("Select() accepted incompatible requirements, want rejection")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestSelectMatchesMultiDeviceTopology(t *testing.T) {
	m := metadataFor("h200-tp2")
	m.GPU = profile.GPU{Type: "H200", Count: 2}
	m.Execution.TensorParallel = 2

	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/tp1:1", "sha256:aaaa"),
		{Image: "registry.example/vcr/tp2:1", Digest: "sha256:cccc", Metadata: m},
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	req := matchingRequirements()
	req.GPUCount = 2
	req.TensorParallel = 2

	selection, err := catalog.Select(req)
	if err != nil {
		t.Fatalf("Select() returned error: %v", err)
	}
	if got, want := selection.ID, "h200-tp2"; got != want {
		t.Errorf("Selection.ID = %q, want %q", got, want)
	}
}

func TestSelectRejectsMissingRequirements(t *testing.T) {
	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/tp1:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	req := matchingRequirements()
	req.GPUType = ""
	_, err = catalog.Select(req)
	if err == nil {
		t.Fatal("Select() accepted requirements without a GPU type, want rejection")
	}
	if !strings.Contains(err.Error(), "gpu.type") {
		t.Errorf("error %q does not name gpu.type", err.Error())
	}

	req = matchingRequirements()
	req.GPUCount = 0
	req.TensorParallel = 0
	_, err = catalog.Select(req)
	if err == nil {
		t.Fatal("Select() accepted requirements without a GPU count, want rejection")
	}
	if !strings.Contains(err.Error(), "gpu.count") {
		t.Errorf("error %q does not name gpu.count", err.Error())
	}
}

func TestSelectRejectsAmbiguousMatches(t *testing.T) {
	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-b", "registry.example/vcr/b:1", "sha256:bbbb"),
		candidate("h200-a", "registry.example/vcr/a:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	_, err = catalog.Select(matchingRequirements())
	if err == nil {
		t.Fatal("Select() picked arbitrarily between two compatible profiles, want rejection")
	}
	for _, sub := range []string{"ambiguous", "h200-a", "h200-b", "exactly one"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
	if strings.Index(err.Error(), "h200-a") > strings.Index(err.Error(), "h200-b") {
		t.Errorf("ambiguous matches are not sorted deterministically: %q", err.Error())
	}
}

func TestSelectListsClosestCandidatesWhenNothingMatches(t *testing.T) {
	nearMiss := metadataFor("near-miss")
	nearMiss.Execution.DType = "float16"

	farMiss := metadataFor("far-miss")
	farMiss.Execution.DType = "float16"
	farMiss.Model.Name = "meta-llama/Llama-3-8B"

	catalog, err := profile.NewCatalog([]profile.Candidate{
		{Image: "registry.example/vcr/far:1", Digest: "sha256:ffff", Metadata: farMiss},
		{Image: "registry.example/vcr/near:1", Digest: "sha256:nnnn", Metadata: nearMiss},
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	_, err = catalog.Select(matchingRequirements())
	if err == nil {
		t.Fatal("Select() accepted incompatible profiles, want rejection")
	}
	for _, sub := range []string{"near-miss", "far-miss", "execution.dtype", "model.name"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
	if strings.Index(err.Error(), "near-miss") > strings.Index(err.Error(), "far-miss") {
		t.Errorf("closest candidates are not ordered by match quality: %q", err.Error())
	}
}

func TestSelectRejectsEmptyCatalog(t *testing.T) {
	catalog, err := profile.NewCatalog(nil)
	if err != nil {
		t.Fatalf("NewCatalog(nil) returned error: %v", err)
	}

	_, err = catalog.Select(matchingRequirements())
	if err == nil {
		t.Fatal("Select() on an empty catalog returned a selection, want rejection")
	}
	if !strings.Contains(err.Error(), "no reconciled profile") {
		t.Errorf("error %q does not explain that no catalog entries exist", err.Error())
	}
}

func TestNewCatalogKeepsRejectedCandidatesAsDiagnostics(t *testing.T) {
	unusable := profile.Candidate{Image: "registry.example/vcr/unusable:1", Metadata: metadataFor("unusable")}
	catalog, err := profile.NewCatalog([]profile.Candidate{
		{Image: "registry.example/vcr/broken:1", Digest: "sha256:bad", Message: "digest resolution failed: unauthorized"},
		unusable,
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	candidates := catalog.Candidates()
	if got, want := len(candidates), 2; got != want {
		t.Fatalf("Candidates() returned %d entries, want %d (rejected entries stay visible)", got, want)
	}

	_, err = catalog.Select(matchingRequirements())
	if err == nil {
		t.Fatal("Select() used an unusable candidate, want rejection")
	}
	for _, sub := range []string{"registry.example/vcr/broken:1", "unauthorized", "registry.example/vcr/unusable:1", "digest"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestNewCatalogRejectsCandidatesWithoutUsableMetadata(t *testing.T) {
	tests := []struct {
		name      string
		candidate profile.Candidate
		wantSub   string
	}{
		{
			name:      "distinct id without metadata",
			candidate: profile.Candidate{Image: "registry.example/vcr/nometa:1", Digest: "sha256:1111"},
			wantSub:   "no profile metadata",
		},
		{
			name:      "unpinned image",
			candidate: profile.Candidate{Image: "registry.example/vcr/unpinned:1", Metadata: metadataFor("unpinned")},
			wantSub:   "digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := profile.NewCatalog([]profile.Candidate{tc.candidate})
			if err != nil {
				t.Fatalf("NewCatalog() returned error: %v", err)
			}

			_, err = catalog.Select(matchingRequirements())
			if err == nil {
				t.Fatal("Select() used a candidate without complete metadata, want rejection")
			}
			for _, sub := range []string{tc.candidate.Image, tc.wantSub} {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestNewCatalogRejectsInvalidMetadataWithoutFailingTheCatalog(t *testing.T) {
	invalid := metadataFor("invalid")
	invalid.SchemaVersion = "vcr.opendatahub-io/profile/v2"

	catalog, err := profile.NewCatalog([]profile.Candidate{
		{Image: "registry.example/vcr/invalid:1", Digest: "sha256:2222", Metadata: invalid},
		candidate("h200-tp1", "registry.example/vcr/valid:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	selection, err := catalog.Select(matchingRequirements())
	if err != nil {
		t.Fatalf("Select() returned error: %v", err)
	}
	if got, want := selection.ID, "h200-tp1"; got != want {
		t.Errorf("Selection.ID = %q, want %q", got, want)
	}

	missing := matchingRequirements()
	missing.GPUType = "A100"
	_, err = catalog.Select(missing)
	if err == nil {
		t.Fatal("Select() accepted an environment with no compatible profile, want rejection")
	}
	if !strings.Contains(err.Error(), "schemaVersion") {
		t.Errorf("error %q does not explain why the invalid profile was excluded", err.Error())
	}
}

func TestNewCatalogCollapsesDuplicateDigestAndID(t *testing.T) {
	entry := candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa")
	catalog, err := profile.NewCatalog([]profile.Candidate{
		entry,
		{Image: "registry.example/vcr/a:latest", Digest: "sha256:aaaa", Metadata: entry.Metadata},
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	selection, err := catalog.Select(matchingRequirements())
	if err != nil {
		t.Fatalf("Select() between identical digests returned error: %v", err)
	}
	if got, want := selection.ID, "h200-tp1"; got != want {
		t.Errorf("Selection.ID = %q, want %q", got, want)
	}
}

func TestNewCatalogRejectsConflictingMetadataForOneDigest(t *testing.T) {
	conflicting := metadataFor("h200-tp1")
	conflicting.Cache.KVBlocks = 4096

	_, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa"),
		{Image: "registry.example/vcr/a:other-tag", Digest: "sha256:aaaa", Metadata: conflicting},
	})
	if err == nil {
		t.Fatal("NewCatalog() accepted conflicting metadata for one digest, want rejection")
	}
	for _, sub := range []string{"h200-tp1", "sha256:aaaa"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestNewCatalogRejectsDuplicateIDWithDifferentDigests(t *testing.T) {
	_, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa"),
		candidate("h200-tp1", "registry.example/vcr/a:2", "sha256:bbbb"),
	})
	if err == nil {
		t.Fatal("NewCatalog() accepted one profile id with two digests, want rejection")
	}
	for _, sub := range []string{"h200-tp1", "sha256:aaaa", "sha256:bbbb"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
}

func TestSelectIgnoresCandidateOrder(t *testing.T) {
	req := matchingRequirements()

	forward, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa"),
		candidate("h200-tp1-copy", "registry.example/vcr/b:1", "sha256:bbbb"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}
	backward, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1-copy", "registry.example/vcr/b:1", "sha256:bbbb"),
		candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	forwardErr := selectError(t, forward, req)
	backwardErr := selectError(t, backward, req)
	if forwardErr != backwardErr {
		t.Errorf("ambiguous-selection errors depend on candidate order:\nforward:  %q\nbackward: %q", forwardErr, backwardErr)
	}
}

func selectError(t *testing.T, catalog *profile.Catalog, req profile.Requirements) string {
	t.Helper()
	_, err := catalog.Select(req)
	if err == nil {
		t.Fatal("Select() returned a selection, want rejection")
	}
	return err.Error()
}

func TestSelectionIsStableForIdenticalBytes(t *testing.T) {
	raw := marshalDocument(t, contractDocument())
	req := matchingRequirements()

	var selections []string
	for i := 0; i < 3; i++ {
		m, err := profile.Parse(raw)
		if err != nil {
			t.Fatalf("Parse() returned error: %v", err)
		}
		catalog, err := profile.NewCatalog([]profile.Candidate{
			{Image: "registry.example/vcr/a:1", Digest: "sha256:aaaa", Metadata: m},
		})
		if err != nil {
			t.Fatalf("NewCatalog() returned error: %v", err)
		}
		selection, err := catalog.Select(req)
		if err != nil {
			t.Fatalf("Select() returned error: %v", err)
		}
		selections = append(selections, selection.ID+"|"+selection.Image+"|"+selection.Digest)
	}

	for i := 1; i < len(selections); i++ {
		if selections[i] != selections[0] {
			t.Errorf("selection %d = %q, want %q", i, selections[i], selections[0])
		}
	}
}

func TestSelectionErrorsDoNotDumpProfileMetadata(t *testing.T) {
	m := metadataFor("h200-tp1")
	m.VLLM.ConfigHash = "sha256:never-in-diagnostics"

	catalog, err := profile.NewCatalog([]profile.Candidate{
		{Image: "registry.example/vcr/a:1", Digest: "sha256:aaaa", Metadata: m},
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	req := matchingRequirements()
	req.DType = "float16"
	_, err = catalog.Select(req)
	if err == nil {
		t.Fatal("Select() accepted an incompatible profile, want rejection")
	}
	if strings.Contains(err.Error(), "sha256:never-in-diagnostics") {
		t.Errorf("error %q dumps profile metadata", err.Error())
	}
}

func TestSelectLimitsCandidateDiagnostics(t *testing.T) {
	var candidates []profile.Candidate
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"} {
		m := metadataFor(id)
		m.Execution.DType = "float16"
		candidates = append(candidates, profile.Candidate{
			Image:    "registry.example/vcr/" + id + ":1",
			Digest:   "sha256:" + id,
			Metadata: m,
		})
	}
	catalog, err := profile.NewCatalog(candidates)
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	_, err = catalog.Select(matchingRequirements())
	if err == nil {
		t.Fatal("Select() accepted incompatible profiles, want rejection")
	}
	for _, sub := range []string{"7 candidate(s) considered", "showing 5 of 7", "p1", "p5"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not name %q", err.Error(), sub)
		}
	}
	if strings.Contains(err.Error(), "p6") || strings.Contains(err.Error(), "p7") {
		t.Errorf("error %q reports more than %d candidates", err.Error(), 5)
	}
}

func TestSelectRejectsNegativeTopologySettings(t *testing.T) {
	catalog, err := profile.NewCatalog([]profile.Candidate{
		candidate("h200-tp1", "registry.example/vcr/a:1", "sha256:aaaa"),
	})
	if err != nil {
		t.Fatalf("NewCatalog() returned error: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*profile.Requirements)
		want   string
	}{
		{"negative tensor parallel", func(r *profile.Requirements) { r.TensorParallel = -1 }, "execution.tensorParallel"},
		{"negative pipeline parallel", func(r *profile.Requirements) { r.PipelineParallel = -1 }, "execution.pipelineParallel"},
		{"negative block size", func(r *profile.Requirements) { r.BlockSize = -1 }, "cache.blockSize"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := matchingRequirements()
			tc.mutate(&req)
			_, err := catalog.Select(req)
			if err == nil {
				t.Fatal("Select() accepted a negative setting, want rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}
