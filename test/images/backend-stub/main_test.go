package main

import "testing"

// TestListenAddressAcceptsPlainSocketAddress pins the address form the real
// simulator accepts: `--control-address HOST:PORT`, handed to
// TcpListener::bind untouched.
func TestListenAddressAcceptsPlainSocketAddress(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "0.0.0.0:8001", want: "0.0.0.0:8001"},
		{raw: "127.0.0.1:8001", want: "127.0.0.1:8001"},
		{raw: ":8001", want: "0.0.0.0:8001"},
		{raw: " 0.0.0.0:8001 ", want: "0.0.0.0:8001"},
	}
	for _, test := range tests {
		got, err := listenAddress(test.raw)
		if err != nil {
			t.Errorf("listenAddress(%q) returned error: %v", test.raw, err)
			continue
		}
		if got != test.want {
			t.Errorf("listenAddress(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}

// TestListenAddressRejectsSchemes asserts the double does not paper over a ZMQ-style
// address, which the real simulator cannot bind.
func TestListenAddressRejectsSchemes(t *testing.T) {
	for _, raw := range []string{"tcp://0.0.0.0:8001", "tcp://*:8001", "http://127.0.0.1:8001"} {
		if got, err := listenAddress(raw); err == nil {
			t.Errorf("listenAddress(%q) = %q, want an error", raw, got)
		}
	}
}

// TestListenEndpointAcceptsZMQEndpoints keeps the KV-event endpoint distinct from
// the control address: that value really is a ZMQ endpoint, and a wildcard host
// binds.
func TestListenEndpointAcceptsZMQEndpoints(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "tcp://*:5556", want: "0.0.0.0:5556"},
		{raw: "tcp://0.0.0.0:5556", want: "0.0.0.0:5556"},
		{raw: "tcp://127.0.0.1:5557", want: "127.0.0.1:5557"},
	}
	for _, test := range tests {
		got, err := listenEndpoint(test.raw)
		if err != nil {
			t.Errorf("listenEndpoint(%q) returned error: %v", test.raw, err)
			continue
		}
		if got != test.want {
			t.Errorf("listenEndpoint(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}
