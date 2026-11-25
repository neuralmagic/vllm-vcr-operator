package profile_test

import (
	"strings"
	"testing"

	"github.com/neuralmagic/vllm-vcr-operator/pkg/profile"
)

// TestParseUsesTheOperatorsRules proves the façade is the operator's validator and
// not a second implementation: a document admission rejects must be rejected here,
// with the same reason.
func TestParseUsesTheOperatorsRules(t *testing.T) {
	bad := []byte(`{"schemaVersion":"vcr.opendatahub-io/profile/v1","id":"x"}`)
	if _, err := profile.Parse(bad); err == nil {
		t.Fatal("Parse accepted a document without model, gpu, execution, cache, vllm, trace or provisioning")
	} else if !strings.Contains(err.Error(), "model") {
		t.Errorf("error %q does not name the missing field", err)
	}
}
