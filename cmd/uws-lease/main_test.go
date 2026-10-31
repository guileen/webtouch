package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateLeaseServerTransport(t *testing.T) {
	tests := []struct {
		name       string
		listen     string
		cert, key  string
		shouldFail bool
	}{
		{name: "loopback cleartext allowed", listen: "127.0.0.1:50152"},
		{name: "ipv6 loopback cleartext allowed", listen: "[::1]:50152"},
		{name: "localhost cleartext allowed", listen: "localhost:50152"},
		{name: "remote cleartext refused", listen: "0.0.0.0:50152", shouldFail: true},
		{name: "remote HTTPS allowed", listen: "0.0.0.0:50152", cert: "server.crt", key: "server.key"},
		{name: "incomplete TLS refused", listen: "127.0.0.1:50152", cert: "server.crt", shouldFail: true},
		{name: "bad listener refused", listen: "not-an-address", shouldFail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateLeaseServerTransport(test.listen, test.cert, test.key)
			if (err != nil) != test.shouldFail {
				t.Fatalf("validateLeaseServerTransport(%q) error = %v, shouldFail=%v", test.listen, err, test.shouldFail)
			}
		})
	}
}

func TestLoadLeaseTLSConfigRejectsBadMaterialBeforeListening(t *testing.T) {
	if _, err := loadLeaseTLSConfig("missing-cert.pem", "missing-key.pem"); err == nil {
		t.Fatal("missing certificate unexpectedly loaded")
	}

	certPath, keyPath := writeTestCertificate(t)
	config, err := loadLeaseTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("loadLeaseTLSConfig(valid pair): %v", err)
	}
	if config.MinVersion != tls.VersionTLS12 || len(config.Certificates) != 1 {
		t.Fatalf("TLS config = %#v, want TLS 1.2+ with one certificate", config)
	}
}

func writeTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "uws-lease-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Minute),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "lease.crt")
	keyPath := filepath.Join(filepath.Dir(certPath), "lease.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
