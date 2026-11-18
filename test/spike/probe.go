package spike

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an HTTP client for one spike surface: the frontend's inference port,
// the supervisor's health endpoint, or the simulator's control API.
//
// The spikes assert everything through these surfaces, because they are what a
// client, kubelet, and llm-d observe. No spike inspects a container's internal
// state.
type Client struct {
	// HTTP is the client to use. Nil uses a client with a short timeout, so a
	// hung surface fails the check instead of the run.
	HTTP *http.Client

	// BaseURL is the scheme, host, and port of the surface.
	BaseURL string
}

// defaultTimeout bounds a single request.
const defaultTimeout = 10 * time.Second

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Get requests path and returns the status and body.
func (c Client) Get(ctx context.Context, path string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// PostJSON sends payload as a JSON request body and returns the status and body.
func (c Client) PostJSON(ctx context.Context, path string, payload any) (int, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("spike: encode request for %s: %w", path, err)
	}
	return c.do(ctx, http.MethodPost, path, encoded)
}

// GetOK requests path and requires a 2xx status, returning the body.
//
// The error carries the status and the (shortened) body, because an unready
// surface explains itself there and that explanation is the diagnostic an
// evidence document records.
func (c Client) GetOK(ctx context.Context, path string) ([]byte, error) {
	status, body, err := c.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("spike: GET %s%s returned %d: %s", c.BaseURL, path, status, snippet(body))
	}
	return body, nil
}

func (c Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("spike: build %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient().Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("spike: %s %s%s: %w", method, c.BaseURL, path, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return response.StatusCode, nil, fmt.Errorf("spike: read %s %s%s: %w", method, c.BaseURL, path, err)
	}
	return response.StatusCode, raw, nil
}

// snippet shortens a response body for an error message.
func snippet(body []byte) string {
	const limit = 400
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}

// Waiter bounds a state-transition wait.
type Waiter struct {
	// Interval is the polling interval.
	Interval time.Duration

	// Timeout is the longest the transition may take.
	Timeout time.Duration
}

// Await polls condition until it reports the state the spike expects, and
// returns how long the transition took and how many observations it made.
//
// A condition reports (done, detail): detail describes what was observed, so a
// timeout fails with the state the run was stuck in rather than a bare deadline.
func Await(ctx context.Context, waiter Waiter, condition func(context.Context) (bool, string)) (time.Duration, int, error) {
	interval := waiter.Interval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	timeout := waiter.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var observed int
	var detail string
	for {
		done, state := condition(ctx)
		observed++
		detail = state
		if done {
			return time.Since(started), observed, nil
		}

		select {
		case <-ctx.Done():
			return time.Since(started), observed, fmt.Errorf(
				"spike: state not reached within %s after %d observations (last: %s): %w",
				timeout, observed, detail, ctx.Err())
		case <-ticker.C:
		}
	}
}

// MetricSamples returns the sample values of one Prometheus series name from a
// text exposition body.
//
// The spikes verify metric *names and meaning*, not a monitoring stack: parsing
// the exposition format is enough to assert that the frontend exposes the series
// llm-d's scorers read, and that the simulator exposes none.
func MetricSamples(body []byte, name string) []float64 {
	var values []float64
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if metricName(fields[0]) != name {
			continue
		}
		var value float64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%g", &value); err == nil {
			values = append(values, value)
		}
	}
	return values
}

// MetricNames returns every sample name in a text exposition body.
func MetricNames(body []byte) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := metricName(fields[0])
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// metricName strips the label set from a Prometheus sample identifier.
func metricName(sample string) string {
	if index := strings.IndexByte(sample, '{'); index >= 0 {
		return sample[:index]
	}
	return sample
}
