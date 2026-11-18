// Package certs generates the self-signed material the cluster acceptance suite
// needs: the operator's webhook serving certificate and the CA that signs the
// test TLS registry's certificate.
//
// It exists so the suite can install the operator without cert-manager: the
// admission registrations are fail-closed, so they need a serving certificate and
// a caBundle before the manager can answer admission requests. The same CA also
// signs the registry certificate so the operator (which resolves profile metadata
// over HTTPS only) trusts it.
//
// Nothing here is a production PKI. Every certificate is short-lived, generated
// into memory, and never written to a persistent location.
package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Bundle is a CA plus one leaf certificate, all PEM encoded.
type Bundle struct {
	// CACert is the PEM-encoded CA certificate, used as the caBundle.
	CACert []byte
	// CAKey is the PEM-encoded CA private key. It is returned so a caller can
	// sign several leaf certificates with one CA.
	CAKey []byte
	// Cert is the PEM-encoded leaf certificate.
	Cert []byte
	// Key is the PEM-encoded leaf private key.
	Key []byte
}

// Options describes one leaf certificate.
type Options struct {
	// CommonName is the leaf's subject common name.
	CommonName string

	// DNSNames are the subject alternative names served as host names.
	DNSNames []string

	// IPAddresses are the subject alternative names served as IP addresses. A
	// kind node address is an IP, so a webhook or registry reached by node IP
	// needs an IP SAN.
	IPAddresses []net.IP

	// Validity is how long the leaf (and CA) remain valid. Default: 24h.
	Validity time.Duration
}

// Generate creates a new CA and a leaf certificate signed by it.
//
// The leaf is valid for server authentication only: these certificates are used
// by TLS servers (the webhook server, the registry), never for client auth.
func Generate(opts Options) (Bundle, error) {
	if opts.CommonName == "" {
		return Bundle{}, fmt.Errorf("certs: a common name is required")
	}
	if len(opts.DNSNames) == 0 && len(opts.IPAddresses) == 0 {
		return Bundle{}, fmt.Errorf(
			"certs: %q needs at least one DNS name or IP address: a certificate without a subject alternative name is not accepted by Go's TLS client",
			opts.CommonName)
	}
	validity := opts.Validity
	if validity == 0 {
		validity = 24 * time.Hour
	}
	notBefore := time.Now().Add(-5 * time.Minute)
	notAfter := notBefore.Add(validity + 5*time.Minute)

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Bundle{}, fmt.Errorf("certs: generate CA key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: opts.CommonName + " CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("certs: create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Bundle{}, fmt.Errorf("certs: parse CA certificate: %w", err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Bundle{}, fmt.Errorf("certs: generate leaf key: %w", err)
	}
	leafSerial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: opts.CommonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     append([]string(nil), opts.DNSNames...),
		IPAddresses:  append([]net.IP(nil), opts.IPAddresses...),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("certs: create leaf certificate: %w", err)
	}

	caKeyPEM, err := pemKey(caKey)
	if err != nil {
		return Bundle{}, err
	}
	leafKeyPEM, err := pemKey(leafKey)
	if err != nil {
		return Bundle{}, err
	}

	return Bundle{
		CACert: pemEncode(caDER, "CERTIFICATE"),
		CAKey:  caKeyPEM,
		Cert:   pemEncode(leafDER, "CERTIFICATE"),
		Key:    leafKeyPEM,
	}, nil
}

// TLSKeyPair returns the leaf as a tls.Certificate, which is what a TLS server
// needs and what the tests use to prove the certificate chains to the CA.
func (b Bundle) TLSKeyPair() (tls.Certificate, error) {
	pair, err := tls.X509KeyPair(b.Cert, b.Key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("certs: load leaf key pair: %w", err)
	}
	return pair, nil
}

// CAPool returns the CA as a certificate pool.
func (b Bundle) CAPool() (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CACert) {
		return nil, fmt.Errorf("certs: CA certificate is not valid PEM")
	}
	return pool, nil
}

// pemKey encodes an RSA private key as PEM.
func pemKey(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("certs: marshal private key: %w", err)
	}
	return pemEncode(der, "PRIVATE KEY"), nil
}

func pemEncode(der []byte, blockType string) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// randomSerial returns a positive 128-bit serial number.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("certs: generate serial number: %w", err)
	}
	return serial, nil
}
