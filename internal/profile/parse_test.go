package profile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// contractDocument mirrors the example metadata document in
// docs/contracts/profile-image.md. Tests mutate a fresh copy per case, so the
// valid baseline is always the documented contract shape.
func contractDocument() map[string]any {
	return map[string]any{
		"schemaVersion": names.ProfileCatalogSchemaVersion,
		"id":            "qwen3-0.6b-h200-tp1-bf16",
		"model": map[string]any{
			"name":              "Qwen/Qwen3-0.6B",
			"revision":          "c9340e6f3",
			"tokenizer":         "Qwen/Qwen3-0.6B",
			"tokenizerRevision": "c9340e6f3",
			"chatTemplateHash":  "sha256:chat-template",
		},
		"gpu": map[string]any{"type": "H200", "count": 1},
		"execution": map[string]any{
			"tensorParallel":   1,
			"pipelineParallel": 1,
			"dtype":            "bfloat16",
			"quantization":     "",
		},
		"cache": map[string]any{
			"blockSize":           64,
			"enablePrefixCaching": true,
			"kvBlocks":            2048,
			"chunkedPrefill":      true,
		},
		"vllm": map[string]any{"version": names.SupportedVLLMVersion, "configHash": "sha256:config"},
		"coverage": map[string]any{
			"maxModelLen":               map[string]any{"min": 1, "max": 131072},
			"maxNumSeqs":                map[string]any{"min": 1, "max": 128},
			"maxNumBatchedTokens":       map[string]any{"min": 1, "max": 8192},
			"longPrefillTokenThreshold": map[string]any{"min": 0, "max": 4096},
			"kvBlocks":                  map[string]any{"min": 1, "max": 2048},
			"schedulingPolicies":        []any{"fcfs", "priority"},
			"kvMemoryConversion":        false,
		},
		"trace": map[string]any{
			"file":           "trace.jsonl.gz",
			"format":         profile.TraceFormatJSONL,
			"recordedTokens": true,
			"latencyModel":   true,
			"kvEvents":       true,
		},
		"provisioning": map[string]any{
			"entrypoint":   []any{"/usr/local/bin/vcr-profile-export"},
			"metadataFile": "metadata.json",
			"traceFile":    "trace.jsonl.gz",
		},
		"capabilities": map[string]any{
			"chatCompletions": true,
			"completions":     true,
			"streaming":       true,
			"metrics":         []any{"vllm:num_requests_running"},
		},
	}
}

func marshalDocument(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal test document: %v", err)
	}
	return raw
}

func parseDocument(t *testing.T, doc map[string]any) (*profile.Metadata, error) {
	t.Helper()
	return profile.Parse(marshalDocument(t, doc))
}

// documentMutations address nested fields by path so cases stay declarative.
func set(doc map[string]any, path []string, value any) {
	target := doc
	for _, key := range path[:len(path)-1] {
		target = target[key].(map[string]any)
	}
	target[path[len(path)-1]] = value
}

func remove(doc map[string]any, path ...string) {
	target := doc
	for _, key := range path[:len(path)-1] {
		target = target[key].(map[string]any)
	}
	delete(target, path[len(path)-1])
}

func TestParseAcceptsContractDocument(t *testing.T) {
	m, err := parseDocument(t, contractDocument())
	if err != nil {
		t.Fatalf("Parse(contract document) returned error: %v", err)
	}

	if got, want := m.SchemaVersion, names.ProfileCatalogSchemaVersion; got != want {
		t.Errorf("SchemaVersion = %q, want %q", got, want)
	}
	if got, want := m.ID, "qwen3-0.6b-h200-tp1-bf16"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
	if got, want := m.Model.Name, "Qwen/Qwen3-0.6B"; got != want {
		t.Errorf("Model.Name = %q, want %q", got, want)
	}
	if got, want := m.Model.Revision, "c9340e6f3"; got != want {
		t.Errorf("Model.Revision = %q, want %q", got, want)
	}
	if got, want := m.GPU.Type, "H200"; got != want {
		t.Errorf("GPU.Type = %q, want %q", got, want)
	}
	if got, want := m.GPU.Count, int32(1); got != want {
		t.Errorf("GPU.Count = %d, want %d", got, want)
	}
	if got, want := m.Execution.DType, "bfloat16"; got != want {
		t.Errorf("Execution.DType = %q, want %q", got, want)
	}
	if got, want := m.Cache.BlockSize, int32(64); got != want {
		t.Errorf("Cache.BlockSize = %d, want %d", got, want)
	}
	if !m.Cache.EnablePrefixCaching {
		t.Error("Cache.EnablePrefixCaching = false, want true")
	}
	if m.Coverage.MaxModelLen == nil {
		t.Fatal("Coverage.MaxModelLen = nil, want declared range")
	}
	if got, want := *m.Coverage.MaxModelLen, (profile.Range{Min: 1, Max: 131072}); got != want {
		t.Errorf("Coverage.MaxModelLen = %+v, want %+v", got, want)
	}
	if got, want := m.Trace.File, "trace.jsonl.gz"; got != want {
		t.Errorf("Trace.File = %q, want %q", got, want)
	}
	if got, want := strings.Join(m.Provisioning.Entrypoint, " "), "/usr/local/bin/vcr-profile-export"; got != want {
		t.Errorf("Provisioning.Entrypoint = %q, want %q", got, want)
	}
	if !m.Capabilities.ChatCompletions {
		t.Error("Capabilities.ChatCompletions = false, want true")
	}
}

func TestParseRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(doc map[string]any)
		wantSub []string
	}{
		{
			name:    "unknown schema version",
			mutate:  func(doc map[string]any) { set(doc, []string{"schemaVersion"}, "vcr.opendatahub-io/profile/v2") },
			wantSub: []string{"schemaVersion", "vcr.opendatahub-io/profile/v2", names.ProfileCatalogSchemaVersion},
		},
		{
			name:    "missing schema version",
			mutate:  func(doc map[string]any) { remove(doc, "schemaVersion") },
			wantSub: []string{"schemaVersion", "required"},
		},
		{
			name:    "missing id",
			mutate:  func(doc map[string]any) { remove(doc, "id") },
			wantSub: []string{"id", "required"},
		},
		{
			name:    "missing model name",
			mutate:  func(doc map[string]any) { remove(doc, "model", "name") },
			wantSub: []string{"model.name", "required"},
		},
		{
			name:    "missing model revision",
			mutate:  func(doc map[string]any) { set(doc, []string{"model", "revision"}, "") },
			wantSub: []string{"model.revision", "required"},
		},
		{
			name:    "missing gpu type",
			mutate:  func(doc map[string]any) { set(doc, []string{"gpu", "type"}, "") },
			wantSub: []string{"gpu.type", "required"},
		},
		{
			name:    "zero gpu count",
			mutate:  func(doc map[string]any) { set(doc, []string{"gpu", "count"}, 0) },
			wantSub: []string{"gpu.count", "greater than 0"},
		},
		{
			name:    "negative gpu count",
			mutate:  func(doc map[string]any) { set(doc, []string{"gpu", "count"}, -2) },
			wantSub: []string{"gpu.count", "greater than 0"},
		},
		{
			name:    "zero tensor parallel",
			mutate:  func(doc map[string]any) { set(doc, []string{"execution", "tensorParallel"}, 0) },
			wantSub: []string{"execution.tensorParallel", "greater than 0"},
		},
		{
			name:    "tensor parallel inconsistent with gpu count",
			mutate:  func(doc map[string]any) { set(doc, []string{"execution", "tensorParallel"}, 2) },
			wantSub: []string{"execution.tensorParallel", "gpu.count"},
		},
		{
			name:    "pipeline parallel greater than one",
			mutate:  func(doc map[string]any) { set(doc, []string{"execution", "pipelineParallel"}, 2) },
			wantSub: []string{"execution.pipelineParallel", "1"},
		},
		{
			name:    "missing pipeline parallel",
			mutate:  func(doc map[string]any) { remove(doc, "execution", "pipelineParallel") },
			wantSub: []string{"execution.pipelineParallel"},
		},
		{
			name:    "empty dtype",
			mutate:  func(doc map[string]any) { set(doc, []string{"execution", "dtype"}, "") },
			wantSub: []string{"execution.dtype", "required"},
		},
		{
			name:    "missing vllm version",
			mutate:  func(doc map[string]any) { remove(doc, "vllm", "version") },
			wantSub: []string{"vllm.version", "required"},
		},
		{
			// The simulator compares the recorded hash verbatim against the
			// trace's meta.config_hash, so a value that is only the schema's
			// algorithm tag would silently disable the check.
			name:    "config hash carries only the algorithm tag",
			mutate:  func(doc map[string]any) { set(doc, []string{"vllm", "configHash"}, "sha256:") },
			wantSub: []string{"vllm.configHash", "config_hash"},
		},
		{
			name:    "non-positive block size",
			mutate:  func(doc map[string]any) { set(doc, []string{"cache", "blockSize"}, 0) },
			wantSub: []string{"cache.blockSize", "greater than 0"},
		},
		{
			name:    "missing trace format",
			mutate:  func(doc map[string]any) { remove(doc, "trace", "format") },
			wantSub: []string{"trace.format", "required"},
		},
		{
			name:    "unknown trace format",
			mutate:  func(doc map[string]any) { set(doc, []string{"trace", "format"}, "vllm-vcr/parquet") },
			wantSub: []string{"trace.format", "vllm-vcr/parquet", profile.TraceFormatJSONL},
		},
		{
			name:    "missing trace file",
			mutate:  func(doc map[string]any) { remove(doc, "trace", "file") },
			wantSub: []string{"trace.file", "required"},
		},
		{
			name:   "trace file does not match declared format",
			mutate: func(doc map[string]any) { set(doc, []string{"trace", "file"}, "trace.parquet") },
			wantSub: []string{
				"trace.file", "trace.parquet", profile.TraceFormatJSONL,
			},
		},
		{
			name:    "missing provisioning entrypoint",
			mutate:  func(doc map[string]any) { remove(doc, "provisioning", "entrypoint") },
			wantSub: []string{"provisioning.entrypoint", "required"},
		},
		{
			name:    "empty provisioning entrypoint",
			mutate:  func(doc map[string]any) { set(doc, []string{"provisioning", "entrypoint"}, []any{}) },
			wantSub: []string{"provisioning.entrypoint", "required"},
		},
		{
			name:    "empty provisioning entrypoint command",
			mutate:  func(doc map[string]any) { set(doc, []string{"provisioning", "entrypoint"}, []any{""}) },
			wantSub: []string{"provisioning.entrypoint"},
		},
		{
			name: "provisioning trace file conflicts with trace file",
			mutate: func(doc map[string]any) {
				set(doc, []string{"provisioning", "traceFile"}, "other.jsonl.gz")
			},
			wantSub: []string{"provisioning.traceFile", "other.jsonl.gz", "trace.file", "trace.jsonl.gz"},
		},
		{
			name: "recorded tokens absent but content replay claimed",
			mutate: func(doc map[string]any) {
				set(doc, []string{"trace", "recordedTokens"}, false)
				set(doc, []string{"capabilities", "contentReplay"}, true)
			},
			wantSub: []string{"trace.recordedTokens", "contentReplay"},
		},
		{
			name: "inverted max model length coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "maxModelLen"}, map[string]any{"min": 131072, "max": 1})
			},
			wantSub: []string{"coverage.maxModelLen", "min", "max"},
		},
		{
			name: "non-positive max num seqs coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "maxNumSeqs"}, map[string]any{"min": 0, "max": 128})
			},
			wantSub: []string{"coverage.maxNumSeqs", "greater than 0"},
		},
		{
			name: "non-positive batched tokens coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "maxNumBatchedTokens"}, map[string]any{"min": 1, "max": 0})
			},
			wantSub: []string{"coverage.maxNumBatchedTokens", "greater than 0"},
		},
		{
			name: "negative long prefill threshold coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "longPrefillTokenThreshold"}, map[string]any{"min": -1, "max": 4096})
			},
			wantSub: []string{"coverage.longPrefillTokenThreshold", "non-negative"},
		},
		{
			name: "empty scheduling policy",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "schedulingPolicies"}, []any{"fcfs", ""})
			},
			wantSub: []string{"coverage.schedulingPolicies[1]", "empty"},
		},
		{
			name: "inverted kv blocks coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "kvBlocks"}, map[string]any{"min": 2048, "max": 1})
			},
			wantSub: []string{"coverage.kvBlocks", "min must not exceed max"},
		},
		{
			name: "non-positive kv blocks coverage",
			mutate: func(doc map[string]any) {
				set(doc, []string{"coverage", "kvBlocks"}, map[string]any{"min": 0, "max": 0})
			},
			wantSub: []string{"coverage.kvBlocks", "greater than 0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := contractDocument()
			tc.mutate(doc)

			_, err := parseDocument(t, doc)
			if err == nil {
				t.Fatal("Parse() accepted an invalid document, want rejection")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestParseAcceptsRandomTokenProfileWithoutContentReplay(t *testing.T) {
	doc := contractDocument()
	set(doc, []string{"trace", "recordedTokens"}, false)

	m, err := parseDocument(t, doc)
	if err != nil {
		t.Fatalf("Parse() of random-token profile returned error: %v", err)
	}
	if m.Trace.RecordedTokens {
		t.Error("Trace.RecordedTokens = true, want false")
	}
	if m.Capabilities.ContentReplay {
		t.Error("Capabilities.ContentReplay = true, want false")
	}
}

func TestParseDefaultsProvisioningFileNames(t *testing.T) {
	doc := contractDocument()
	remove(doc, "provisioning", "metadataFile")
	remove(doc, "provisioning", "traceFile")

	m, err := parseDocument(t, doc)
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}
	if got, want := m.Provisioning.MetadataFile, names.ProfileMetadataFileName; got != want {
		t.Errorf("Provisioning.MetadataFile = %q, want %q", got, want)
	}
	if got, want := m.Provisioning.TraceFile, m.Trace.File; got != want {
		t.Errorf("Provisioning.TraceFile = %q, want %q", got, want)
	}
}

func TestParseAcceptsProfileWithoutCoverage(t *testing.T) {
	doc := contractDocument()
	remove(doc, "coverage")

	m, err := parseDocument(t, doc)
	if err != nil {
		t.Fatalf("Parse() rejected a profile that declares no override coverage: %v", err)
	}
	if m.Coverage.MaxModelLen != nil || m.Coverage.MaxNumSeqs != nil || m.Coverage.KVBlocks != nil {
		t.Error("Coverage ranges were populated from an absent coverage block")
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	_, err := profile.Parse([]byte("{"))
	if err == nil {
		t.Fatal("Parse() accepted malformed JSON, want error")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error %q does not name the profile metadata document", err.Error())
	}
}

func TestParseErrorDoesNotEchoDocumentValues(t *testing.T) {
	doc := contractDocument()
	// A profile image may carry configuration that must never surface in
	// diagnostics. Errors name contract elements, never document contents.
	doc["schemaVersion"] = "vcr.opendatahub-io/profile/v2"
	doc["pullCredentials"] = "hunter2-do-not-log"

	_, err := parseDocument(t, doc)
	if err == nil {
		t.Fatal("Parse() accepted an unsupported schema version, want rejection")
	}
	if strings.Contains(err.Error(), "hunter2-do-not-log") {
		t.Errorf("error %q echoes document values", err.Error())
	}
}

func TestParseIsStable(t *testing.T) {
	raw := marshalDocument(t, contractDocument())

	first, err := profile.Parse(raw)
	if err != nil {
		t.Fatalf("first Parse() returned error: %v", err)
	}
	second, err := profile.Parse(raw)
	if err != nil {
		t.Fatalf("second Parse() returned error: %v", err)
	}

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first metadata: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second metadata: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("Parse() is not deterministic:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}
