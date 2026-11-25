package certs_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/neuralmagic/vllm-vcr-operator/test/cluster/certs"
)

// TestGenerateServerCertificateChainsToCA asserts the property the suite relies
// on: a TLS server using the generated leaf is accepted by a client that trusts
// only the generated CA, for both a DNS name (the webhook Service) and an IP
// address (a kind node address for the registry).
func TestGenerateServerCertificateChainsToCA(t *testing.T) {
	bundle, err := certs.Generate(certs.Options{
		CommonName:  "vllm-vcr-operator-webhook-service.vllm-vcr-operator-system.svc",
		DNSNames:    []string{"vllm-vcr-operator-webhook-service.vllm-vcr-operator-system.svc"},
		IPAddresses: []net.IP{net.ParseIP("10.89.0.4")},
		Validity:    time.Hour,
	})
	if err != nil {
		t.Fatalf("certs.Generate: %v", err)
	}
	if len(bundle.CACert) == 0 || len(bundle.Cert) == 0 || len(bundle.Key) == 0 || len(bundle.CAKey) == 0 {
		t.Fatal("certs.Generate returned an incomplete bundle")
	}

	pool, err := bundle.CAPool()
	if err != nil {
		t.Fatalf("CAPool: %v", err)
	}
	leaf, err := x509.ParseCertificate(mustKeyPair(t, bundle).Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	for _, name := range []string{
		"vllm-vcr-operator-webhook-service.vllm-vcr-operator-system.svc",
		"10.89.0.4",
	} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("leaf does not verify for %q: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "wrong.example", Roots: pool}); err == nil {
		t.Error("leaf verified for a name it was not issued for: the SAN list is not being applied")
	}
	if leaf.IsCA {
		t.Error("leaf is a CA certificate: the serving certificate must not sign others")
	}
}

// TestGenerateRequiresASubjectAlternativeName asserts the rejection that keeps a
// silently unusable certificate out of a test run: Go's TLS client ignores the
// legacy common-name field, so a certificate without a SAN can never validate.
func TestGenerateRequiresASubjectAlternativeName(t *testing.T) {
	if _, err := certs.Generate(certs.Options{CommonName: "no-san"}); err == nil {
		t.Fatal("certs.Generate accepted an options without any DNS name or IP: a SAN-less certificate never validates")
	}
	if _, err := certs.Generate(certs.Options{}); err == nil {
		t.Fatal("certs.Generate accepted an empty options")
	}
}

func mustKeyPair(t *testing.T, bundle certs.Bundle) tls.Certificate {
	t.Helper()
	pair, err := bundle.TLSKeyPair()
	if err != nil {
		t.Fatalf("TLSKeyPair: %v", err)
	}
	return pair
}
