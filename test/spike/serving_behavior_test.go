package spike_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// completionResponse is the part of the completions response the spike asserts.
type completionResponse struct {
	Choices []struct {
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// tokenizeResponse is the frontend's tokenizer endpoint response.
type tokenizeResponse struct {
	Tokens []int `json:"tokens"`
	Count  int   `json:"count"`
}

// TestServingBehavior is the serving-behavior and llm-d contract spike the
// specification requires: it drives the frontend's HTTP API and establishes
// generation, streaming, tokenizer consistency, metric names and semantics,
// random versus recorded tokens, unmatched replay behavior, cache pressure, and
// KV-event publication.
//
// It runs only when VCR_FRONTEND_IMAGE, VCR_BACKEND_IMAGE, and a recorded profile
// are configured; VCR_SPIKE_REQUIRED=1 turns a missing environment into a
// failure. The run writes test/spike/evidence/serving-behavior.json, which is the
// only evidence docs/spikes/serving-behavior.md may cite.
//
// A check that fails here is a capability finding. The specification's rule is
// that a failed capability becomes an explicit rejection in
// docs/capability-matrix.md or identified implementation work, never an
// undocumented fallback.
func TestServingBehavior(t *testing.T) {
	env := spike.Gate(t, os.Getenv, exec.LookPath)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	stack := spike.NewStack(t, env)
	defer stack.Close(ctx)

	evidence := spike.NewEvidence("serving-behavior", env)
	defer func() {
		if err := evidence.Write(spike.EvidencePath("serving-behavior")); err != nil {
			t.Errorf("write evidence: %v", err)
		}
	}()

	err := stack.Provision(ctx)
	evidence.Check("the profile image's provisioning entrypoint exports into the profile volume", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}
	if env.ProfileImage == "" {
		evidence.RecordGap("profile provisioning: the run served an already-exported profile directory, so the image's provisioning entrypoint was not exercised")
	}
	err = stack.LoadProfile()
	evidence.Check("the exported profile metadata is valid and its declared trace is present", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}

	// KV events are enabled so that publication is observable. Whether the
	// profile records events is a separate condition the operator enforces; the
	// spike records what the capability costs.
	stack.EnableKVEvents()
	settings := stack.Settings()
	metadata := stack.Metadata()

	err = stack.StartFrontend(ctx)
	evidence.Check("the frontend container starts", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}
	err = stack.StartBackend(ctx)
	evidence.Check("the simulator container starts", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}
	_, err = stack.AwaitReady(ctx)
	evidence.Check("the serving runtime reaches readiness", err)
	if err != nil {
		evidence.Observe("stack diagnostics", stack.Diagnostics())
		return
	}

	inference := stack.Inference()
	model := settings.Model
	prompt := "The prefix cache routes by block hash. "

	// Model identity: llm-d discovers pods through /v1/models, so the served
	// model must be the profile's model, not an operator alias.
	models, err := inference.GetOK(ctx, "/v1/models")
	evidence.Check("the inference port serves model discovery", err)
	if err == nil {
		evidence.Check("model discovery reports the profile's model", requireModelID(models, model))
	}

	// Tokenization consistency: rendering and generation must use one tokenizer.
	// The check compares two independent surfaces rather than a second tokenizer.
	promptTokens, tokenizeErr := tokenize(ctx, inference, model, prompt)
	if errors.Is(tokenizeErr, errTokenizeUnavailable) {
		evidence.RecordGap("tokenizer consistency: this frontend serves no /tokenize endpoint, so token-id consistency was not demonstrated")
		tokenizeErr = nil
	}
	evidence.Check("the frontend tokenizes a fixed prompt deterministically", tokenizeErr)
	if tokenizeErr == nil && len(promptTokens) > 0 {
		evidence.Observe("tokenized prompt length", strconv.Itoa(len(promptTokens)))
	}

	// Generation.
	completion, err := complete(ctx, inference, model, prompt, 8, false)
	evidence.Check("a completion request is generated", err)
	if err == nil {
		evidence.Check("the completion carries generated text", requireText(completion))
		if tokenizeErr == nil {
			evidence.Check("reported prompt_tokens equals the tokenized prompt length", requirePromptTokens(completion, len(promptTokens)))
		}
	}

	// Streaming: the same endpoint must deliver partial output.
	stream, err := completeRaw(ctx, inference, model, prompt, 8)
	evidence.Check("a streaming completion request is served", err)
	if err == nil {
		events := countSSEEvents(stream)
		evidence.Observe("streaming event count", strconv.Itoa(events))
		evidence.Check("streaming delivers more than one server-sent event and terminates with [DONE]", requireStreaming(stream, events))
	}

	// Metrics: the frontend exposes Prometheus series; the simulator exposes none
	// (its control API is JSON and has no /metrics handler).
	metrics, err := inference.GetOK(ctx, "/metrics")
	evidence.Check("the frontend exposes Prometheus metrics on the inference port", err)
	if err == nil {
		names := spike.MetricNames(metrics)
		evidence.Observe("vllm metric series", strings.Join(vllmSeries(names), ", "))
		evidence.Check("the metric set llm-d's scorers read is exposed", requireMetricNames(names, "vllm:num_requests_running", "vllm:kv_cache_usage_perc"))
	}

	controlStatus, controlBody, controlErr := stack.Control().Get(ctx, "/metrics")
	evidence.Check("the simulator's control API is reachable and has no metrics surface", func() error {
		if controlErr != nil {
			return controlErr
		}
		if controlStatus == 200 {
			return fmt.Errorf("/metrics returned 200: the simulator would be claiming a metrics surface it does not have")
		}
		return nil
	}())
	evidence.Observe("simulator /metrics status", fmt.Sprintf("%d: %s", controlStatus, truncate(string(controlBody), 120)))

	// Random versus recorded tokens. Which one this run produces is the profile's
	// decision, and the pinned simulator consumes each recorded record once, so
	// repeating a prompt is not proof of replay. The harness therefore records the
	// observation and reports the strictly checkable fact: the engine's own request
	// accounting. Content assertions are authorized only for a profile that grants
	// contentReplay, and even then a match rate has to come from the counters.
	first, firstErr := complete(ctx, inference, model, prompt, 8, false)
	second, secondErr := complete(ctx, inference, model, prompt, 8, false)
	if firstErr == nil && secondErr == nil {
		identical := text(first) == text(second)
		if metadata.Capabilities.ContentReplay {
			evidence.Observe("content replay", fmt.Sprintf(
				"profile grants contentReplay; two identical prompts produced identical text: %t (each recorded record is consumed once, so a repeat may legitimately fall back to random tokens)", identical))
		} else {
			evidence.RecordGap("content-identical replay: the profile grants no capabilities.contentReplay, so this run's tokens are seeded random output and content assertions are not authorized")
			evidence.Observe("random token behavior", fmt.Sprintf("two identical prompts produced identical text: %t (the simulator seeds random tokens per request id)", identical))
		}
	}

	// Unmatched replay behavior: a prompt the trace cannot match must still be
	// served, and the simulator reports no failure signal for it.
	novel := fmt.Sprintf("unmatched-%d: a prompt no trace record contains", time.Now().UnixNano())
	unmatched, err := complete(ctx, inference, model, novel, 4, false)
	evidence.Check("an unmatched prompt is still served", err)
	if err == nil {
		evidence.Check("the unmatched prompt returns tokens", requireText(unmatched))
	}
	evidence.RecordGap("unmatched-request signalling: the pinned simulator falls back to random tokens without an error signal, so a content assertion cannot distinguish a replay hit from a fallback by response alone")

	// Engine-side accounting: the simulator's control API is the authoritative
	// witness that requests reached the engine once, completed, and did not fail.
	sent := 5 // the completions issued above: generation, streaming, two repeats, unmatched
	stats, statsErr := readStats(ctx, stack.Control())
	evidence.Check("the simulator's request counters account for every request", func() error {
		if statsErr != nil {
			return statsErr
		}
		if stats.Received < sent {
			return fmt.Errorf("requests_received = %d, but the harness issued %d completions: requests were lost before the engine", stats.Received, sent)
		}
		if stats.Failed != 0 {
			return fmt.Errorf("requests_failed = %d: the engine reported failures the responses did not", stats.Failed)
		}
		if settled := stats.Completed + stats.Failed + stats.Aborted; settled > stats.Received {
			return fmt.Errorf("received %d < completed %d + failed %d + aborted %d", stats.Received, stats.Completed, stats.Failed, stats.Aborted)
		}
		return nil
	}())
	evidence.Observe("simulator request counters", fmt.Sprintf("received=%d completed=%d failed=%d aborted=%d",
		stats.Received, stats.Completed, stats.Failed, stats.Aborted))

	// Prefix-cache pressure: capacity is the profile's KV block count, and
	// eviction must be observable through the frontend's cache-usage series.
	maxUsage, err := driveCachePressure(ctx, stack, model)
	evidence.Check("KV-cache usage reaches the profile's capacity under load", err)
	if err == nil {
		evidence.Observe("maximum vllm:kv_cache_usage_perc observed", fmt.Sprintf("%.3f", maxUsage))
	}

	// KV-event publication: the simulator binds the port llm-d discovers.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	var dialer net.Dialer
	conn, dialErr := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(names.KVEventsPort)))
	evidence.Check("the simulator binds the KV-event port llm-d's discovery reads", dialErr)
	if dialErr == nil {
		conn.Close()
	} else {
		evidence.RecordGap("KV-event publication: no publisher accepted a connection on the discovery port, so no cache-event behavior was demonstrated")
	}

	// What this run cannot establish, whatever the checks above reported.
	evidence.RecordGap("KV-event wire format: the harness links no ZMQ client, so frames were not decoded; frame-level validation against the real consumer is the cluster suite's job (make test-cluster)")
	evidence.RecordGap("KV-event replay: the simulator's ready response reports replay_endpoint null and publishes no replay responder, so router re-indexing after a lost event window is not demonstrated. The operator rejects an explicit replay endpoint unless a profile grants capabilities.kvEventReplay")
	evidence.RecordGap("llm-d routing: no router, EPP, or discovery component ran in this spike, so cache-local routing and eviction effects on routing are not established")
	evidence.RecordGap("model output quality: outputs are recorded ids or seeded random tokens; no comparison with real inference output was made")
	evidence.RecordGap("GPU behavior and timing fidelity: no accelerator ran, and the recorded latency was served without comparison to a fresh capture")

	if evidence.Failed() {
		t.Errorf("a serving capability did not hold for the pinned images; record it in docs/capability-matrix.md as rejected or as identified work. See %s\n%s",
			spike.EvidencePath("serving-behavior"), stack.Diagnostics())
	}
}

// errTokenizeUnavailable reports that the frontend serves no tokenizer endpoint.
var errTokenizeUnavailable = errors.New("tokenizer endpoint is not served")

// engineStats is the simulator's request accounting.
type engineStats struct {
	Received  int `json:"requests_received"`
	Completed int `json:"requests_completed"`
	Failed    int `json:"requests_failed"`
	Aborted   int `json:"requests_aborted"`
	Running   int `json:"running"`
	Waiting   int `json:"waiting"`
}

// readStats reads the simulator's request counters.
func readStats(ctx context.Context, control spike.Client) (engineStats, error) {
	body, err := control.GetOK(ctx, "/stats")
	if err != nil {
		return engineStats{}, err
	}
	var stats engineStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return engineStats{}, fmt.Errorf("decode /stats: %w", err)
	}
	return stats, nil
}

// tokenize asks the frontend to tokenize prompt.
func tokenize(ctx context.Context, client spike.Client, model, prompt string) ([]int, error) {
	status, body, err := client.PostJSON(ctx, "/tokenize", map[string]any{"model": model, "prompt": prompt})
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, fmt.Errorf("%w: %s", errTokenizeUnavailable, truncate(string(body), 200))
	}
	if status != 200 {
		return nil, fmt.Errorf("/tokenize returned %d: %s", status, truncate(string(body), 200))
	}
	var decoded tokenizeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode /tokenize response: %w", err)
	}
	if len(decoded.Tokens) == 0 {
		return nil, fmt.Errorf("/tokenize returned no token ids")
	}
	return decoded.Tokens, nil
}

// complete issues a non-streaming completion request.
func complete(ctx context.Context, client spike.Client, model, prompt string, maxTokens int, stream bool) (*completionResponse, error) {
	status, body, err := client.PostJSON(ctx, "/v1/completions", map[string]any{
		"model":       model,
		"prompt":      prompt,
		"max_tokens":  maxTokens,
		"temperature": 0,
	})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("/v1/completions returned %d: %s", status, truncate(string(body), 400))
	}
	var decoded completionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode /v1/completions response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return nil, fmt.Errorf("/v1/completions returned no choices")
	}
	return &decoded, nil
}

// completeRaw issues a streaming completion request and returns the raw SSE
// body.
func completeRaw(ctx context.Context, client spike.Client, model, prompt string, maxTokens int) (string, error) {
	status, body, err := client.PostJSON(ctx, "/v1/completions", map[string]any{
		"model":       model,
		"prompt":      prompt,
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      true,
	})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("/v1/completions (stream) returned %d: %s", status, truncate(string(body), 400))
	}
	return string(body), nil
}

// driveCachePressure sends prompts until the frontend reports full KV-cache
// usage, and returns the highest usage observed.
func driveCachePressure(ctx context.Context, stack *spike.Stack, model string) (float64, error) {
	settings := stack.Settings()
	// One request holds roughly a block per TokensPerBlock prompt tokens. A
	// distinct leading token keeps prefix caching from collapsing the load into
	// one stored block chain.
	filler := strings.Repeat("quantization calibration placeholder ", int(settings.TokensPerBlock)/2)

	var maxUsage float64
	const attempts = 120
	for i := 0; i < attempts; i++ {
		prompt := fmt.Sprintf("pressure-%d %s", i, filler)
		if _, err := complete(ctx, stack.Inference(), model, prompt, 1, false); err != nil {
			return maxUsage, err
		}
		body, err := stack.Inference().GetOK(ctx, "/metrics")
		if err != nil {
			return maxUsage, err
		}
		for _, value := range spike.MetricSamples(body, "vllm:kv_cache_usage_perc") {
			if value > maxUsage {
				maxUsage = value
			}
		}
		if maxUsage >= 0.95 {
			return maxUsage, nil
		}
	}
	return maxUsage, fmt.Errorf("KV-cache usage peaked at %.3f after %d requests: eviction pressure was not reached", maxUsage, attempts)
}

// requireModelID asserts model discovery reports id.
func requireModelID(body []byte, id string) error {
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("decode /v1/models response: %w", err)
	}
	for _, entry := range decoded.Data {
		if entry.ID == id {
			return nil
		}
	}
	return fmt.Errorf("/v1/models does not list %q: %s", id, truncate(string(body), 300))
}

// requireText asserts a completion carries generated text.
func requireText(completion *completionResponse) error {
	if completion == nil || len(completion.Choices) == 0 {
		return fmt.Errorf("no choices")
	}
	if strings.TrimSpace(completion.Choices[0].Text) == "" {
		return fmt.Errorf("the first choice carries no text")
	}
	return nil
}

// requirePromptTokens asserts the reported prompt length matches the tokenizer's.
func requirePromptTokens(completion *completionResponse, want int) error {
	if completion.Usage.PromptTokens != want {
		return fmt.Errorf("usage.prompt_tokens = %d, tokenizer produced %d ids: the two surfaces disagree", completion.Usage.PromptTokens, want)
	}
	return nil
}

// requireStreaming asserts an SSE body has several events and a terminator.
func requireStreaming(body string, events int) error {
	if events < 2 {
		return fmt.Errorf("%d server-sent events: partial output was not demonstrated", events)
	}
	if !strings.Contains(body, "[DONE]") {
		return fmt.Errorf("the event stream does not terminate with [DONE]")
	}
	return nil
}

// requireMetricNames asserts every wanted series is present.
func requireMetricNames(names []string, want ...string) error {
	present := map[string]struct{}{}
	for _, name := range names {
		present[name] = struct{}{}
	}
	var missing []string
	for _, name := range want {
		if _, ok := present[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("metrics are missing %s (exposed: %d series)", strings.Join(missing, ", "), len(names))
	}
	return nil
}

// countSSEEvents counts the data lines of a server-sent event body.
func countSSEEvents(body string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			count++
		}
	}
	return count
}

// text is the first choice's text.
func text(completion *completionResponse) string {
	if completion == nil || len(completion.Choices) == 0 {
		return ""
	}
	return completion.Choices[0].Text
}

// vllmSeries returns the vllm:* series names from a metric name list.
func vllmSeries(names []string) []string {
	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, "vllm:") {
			out = append(out, name)
		}
	}
	return out
}
