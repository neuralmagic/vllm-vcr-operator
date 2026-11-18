package spike_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendatahub-io/vllm-vcr-operator/test/spike"
)

// TestAwaitReturnsWhenTheConditionHolds asserts the harness's transition
// assertion: a spike step records how long a state change took, so waiting must
// report the elapsed time and the number of attempts it observed.
func TestAwaitReturnsWhenTheConditionHolds(t *testing.T) {
	var attempts int32
	elapsed, observed, err := spike.Await(context.Background(), spike.Waiter{Interval: time.Millisecond, Timeout: 2 * time.Second},
		func(context.Context) (bool, string) {
			if atomic.AddInt32(&attempts, 1) < 3 {
				return false, "engine not registered"
			}
			return true, "engine registered"
		})
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if observed != 3 {
		t.Errorf("observed %d attempts, want 3", observed)
	}
	if elapsed <= 0 {
		t.Errorf("elapsed = %v, want a measured duration", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want less than the timeout", elapsed)
	}
}

// TestAwaitFailsWithTheLastObservation asserts a transition that never happened
// fails with what was actually observed, because an evidence document must
// record the state the run was stuck in.
func TestAwaitFailsWithTheLastObservation(t *testing.T) {
	_, observed, err := spike.Await(context.Background(), spike.Waiter{Interval: time.Millisecond, Timeout: 20 * time.Millisecond},
		func(context.Context) (bool, string) {
			return false, "ready marker absent"
		})
	if err == nil {
		t.Fatal("Await reported a transition that never happened")
	}
	if !strings.Contains(err.Error(), "ready marker absent") {
		t.Errorf("error does not carry the last observation:\n%s", err)
	}
	if observed == 0 {
		t.Error("Await reported zero observations")
	}
}

// TestGetOKReportsStatusAndBody asserts an unready endpoint fails with the
// status and the body, which is the diagnostic a spike records when readiness
// never arrives.
func TestGetOKReportsStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "engine not registered", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := spike.Client{HTTP: server.Client(), BaseURL: server.URL}
	_, err := client.GetOK(context.Background(), "/v1/models")
	if err == nil {
		t.Fatal("GetOK accepted a 503 response")
	}
	message := err.Error()
	if !strings.Contains(message, "503") || !strings.Contains(message, "engine not registered") {
		t.Errorf("error does not report the status and body:\n%s", message)
	}
}

// TestPostJSONRoundTrips asserts a serving check can drive the frontend HTTP API
// the way a client does: JSON in, JSON out.
func TestPostJSONRoundTrips(t *testing.T) {
	type request struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	type response struct {
		ID string `json:"id"`
	}

	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q, want JSON", got)
		}
		body, _ := io.ReadAll(r.Body)
		seen = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cmpl-1"}`)
	}))
	defer server.Close()

	client := spike.Client{HTTP: server.Client(), BaseURL: server.URL}
	status, raw, err := client.PostJSON(context.Background(), "/v1/completions", request{Model: "Qwen/Qwen3-0.6B", Prompt: "hello"})
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if !strings.Contains(seen, `"prompt":"hello"`) {
		t.Errorf("request body = %s", seen)
	}
	var decoded response
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.ID != "cmpl-1" {
		t.Errorf("decoded id = %q", decoded.ID)
	}
}

// TestClientKeepsTheResponseOnAnErrorStatus asserts the caller can inspect a
// response body of a failing request: the simulator's control API and the
// frontend both explain failures in the body.
func TestClientKeepsTheResponseOnAnErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "404 page not found")
	}))
	defer server.Close()

	client := spike.Client{HTTP: server.Client(), BaseURL: server.URL}
	status, raw, err := client.Get(context.Background(), "/metrics")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 from the simulator control API", status)
	}
	if !strings.Contains(string(raw), "404") {
		t.Errorf("body = %q", raw)
	}
}

// TestAwaitStopsWhenTheContextIsCancelled asserts a harness that is shutting
// down does not wait out the timeout.
func TestAwaitStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := spike.Await(ctx, spike.Waiter{Interval: time.Millisecond, Timeout: time.Hour},
		func(context.Context) (bool, string) { return false, "still not ready" })
	if err == nil {
		t.Fatal("Await ignored context cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error = %v, want it to report the cancellation", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Await waited %v after cancellation", time.Since(start))
	}
}

// TestMetricSamplesReadsTheExpositionFormat asserts metric assertions read the
// text exposition format the frontend serves: series names with label sets, one
// value per sample, comments ignored.
func TestMetricSamplesReadsTheExpositionFormat(t *testing.T) {
	body := []byte(`# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means full.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen3-0.6B"} 0.25
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen3-0.6B"} 2.0
vllm:prefix_cache_hits_total{engine="0"} 7.0
vllm:prefix_cache_hits_total{engine="1"} 3.0
`)

	if got := spike.MetricSamples(body, "vllm:kv_cache_usage_perc"); len(got) != 1 || got[0] != 0.25 {
		t.Errorf("kv_cache_usage samples = %v, want [0.25]", got)
	}
	if got := spike.MetricSamples(body, "vllm:prefix_cache_hits_total"); len(got) != 2 || got[0] != 7 || got[1] != 3 {
		t.Errorf("prefix_cache_hits samples = %v, want [7 3]", got)
	}
	if got := spike.MetricSamples(body, "vllm:does_not_exist"); len(got) != 0 {
		t.Errorf("absent series returned %v", got)
	}

	names := spike.MetricNames(body)
	for _, want := range []string{"vllm:kv_cache_usage_perc", "vllm:num_requests_running", "vllm:prefix_cache_hits_total"} {
		if !slices.Contains(names, want) {
			t.Errorf("MetricNames does not contain %q: %v", want, names)
		}
	}
	if slices.Contains(names, "# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means full.") {
		t.Error("MetricNames kept a comment line")
	}
}
