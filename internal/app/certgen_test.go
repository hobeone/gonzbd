package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("generateSelfSigned: %v", err)
	}

	// Verify cert PEM is valid
	if len(certPEM) == 0 {
		t.Fatal("certPEM is empty")
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		t.Fatal("failed to decode cert PEM")
	}
	if certBlock.Type != "CERTIFICATE" {
		t.Errorf("cert type = %q; want CERTIFICATE", certBlock.Type)
	}

	// Parse certificate
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	// Verify CN is "gonzbd"
	if cert.Subject.CommonName != "gonzbd" {
		t.Errorf("CN = %q; want gonzbd", cert.Subject.CommonName)
	}

	// Verify DNS SANs
	dnsNames := make(map[string]bool)
	for _, name := range cert.DNSNames {
		dnsNames[name] = true
	}
	if !dnsNames["localhost"] {
		t.Error("missing localhost in SANs")
	}

	// Verify IP SANs
	ipAddrs := make(map[string]bool)
	for _, ip := range cert.IPAddresses {
		ipAddrs[ip.String()] = true
	}
	if !ipAddrs["127.0.0.1"] {
		t.Error("missing 127.0.0.1 in SANs")
	}
	if !ipAddrs["::1"] {
		t.Error("missing ::1 in SANs")
	}

	// Verify validity period
	now := time.Now()
	if cert.NotBefore.After(now) {
		t.Errorf("NotBefore is in future: %v", cert.NotBefore)
	}
	expectedNotAfter := now.AddDate(5, 0, 0)
	// Allow 2 minutes drift
	drift := 2 * time.Minute
	if cert.NotAfter.Before(expectedNotAfter.Add(-drift)) || cert.NotAfter.After(expectedNotAfter.Add(drift)) {
		t.Errorf("NotAfter = %v; want ~%v", cert.NotAfter, expectedNotAfter)
	}

	// Verify key PEM
	if len(keyPEM) == 0 {
		t.Fatal("keyPEM is empty")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("failed to decode key PEM")
	}
	if keyBlock.Type != "PRIVATE KEY" {
		t.Errorf("key type = %q; want PRIVATE KEY", keyBlock.Type)
	}

	// Parse private key
	privKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}

	edKey, ok := privKey.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("private key type = %T; want ed25519.PrivateKey", privKey)
	}

	if len(edKey) != ed25519.PrivateKeySize {
		t.Errorf("key size = %d bytes; want %d", len(edKey), ed25519.PrivateKeySize)
	}

	if cert.SignatureAlgorithm != x509.PureEd25519 {
		t.Errorf("signature algorithm = %v; want PureEd25519", cert.SignatureAlgorithm)
	}

	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}

	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert public key type = %T; want ed25519.PublicKey", cert.PublicKey)
	}
	if !certPub.Equal(edKey.Public()) {
		t.Error("certificate public key does not match private key")
	}
}

func TestGenerateSelfSignedUniqueness(t *testing.T) {
	t.Parallel()

	// Generate two certificates and verify they have different serial numbers
	cert1PEM, _, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("generateSelfSigned: %v", err)
	}

	cert2PEM, _, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("generateSelfSigned: %v", err)
	}

	block1, _ := pem.Decode(cert1PEM)
	block2, _ := pem.Decode(cert2PEM)

	cert1, _ := x509.ParseCertificate(block1.Bytes)
	cert2, _ := x509.ParseCertificate(block2.Bytes)

	if cert1.SerialNumber.Cmp(cert2.SerialNumber) == 0 {
		t.Error("generated certificates have same serial number")
	}
}

func TestWriteSelfSigned(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	certPath := tmpDir + "/cert.pem"
	keyPath := tmpDir + "/key.pem"

	err := WriteSelfSigned(certPath, keyPath)
	if err != nil {
		t.Fatalf("WriteSelfSigned: %v", err)
	}

	// Verify cert file exists and can be parsed
	certPEM := readFile(t, certPath)
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode cert from file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate from file: %v", err)
	}

	// Basic sanity check
	if cert.Subject.CommonName != "gonzbd" {
		t.Errorf("CN from file = %q; want gonzbd", cert.Subject.CommonName)
	}

	// Verify key file exists and can be parsed
	keyPEM := readFile(t, keyPath)
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("failed to decode key from file")
	}
	privKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse key from file: %v", err)
	}

	if _, ok := privKey.(ed25519.PrivateKey); !ok {
		t.Fatalf("key type = %T; want ed25519.PrivateKey", privKey)
	}

	// Verify file permissions
	certInfo := statFile(t, certPath)
	if perm := certInfo.Mode().Perm(); perm != 0o644 {
		t.Errorf("cert file permissions = %#o; want %#o", perm, 0o644)
	}

	keyInfo := statFile(t, keyPath)
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file permissions = %#o; want %#o", perm, 0o600)
	}
}

func TestWriteSelfSignedCreatesDirs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	certPath := tmpDir + "/nested/deep/cert.pem"
	keyPath := tmpDir + "/nested/deep/key.pem"

	err := WriteSelfSigned(certPath, keyPath)
	if err != nil {
		t.Fatalf("WriteSelfSigned: %v", err)
	}

	// Verify both files exist
	if readFile(t, certPath) == nil {
		t.Fatal("cert file not found")
	}
	if readFile(t, keyPath) == nil {
		t.Fatal("key file not found")
	}
}

// Helper functions

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return info
}

// ---------- Direct Certgen Helpers ----------

func TestWriteFileAtomic_Direct(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := tmp + "/nested/file.txt"
	data := []byte("atomic-data")

	if err := writeFileAtomic(path, data, 0o644); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	got := readFile(t, path)
	if string(got) != "atomic-data" {
		t.Errorf("got %q, want atomic-data", got)
	}

	info := statFile(t, path)
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("perm = %#o, want %#o", perm, 0o644)
	}
}

func TestWriteSelfSignedErrors(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a regular file to block directory creation/writing.
	blockerFile := filepath.Join(tmpDir, "blocked_path")
	if err := os.WriteFile(blockerFile, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	t.Run("cert_write_error", func(t *testing.T) {
		// Attempting to write where the parent dir is a regular file.
		badCertPath := filepath.Join(blockerFile, "cert.pem")
		badKeyPath := filepath.Join(tmpDir, "key.pem")
		err := WriteSelfSigned(badCertPath, badKeyPath)
		if err == nil {
			t.Fatal("expected error writing to invalid cert path, got nil")
		}
		if !strings.Contains(err.Error(), "write certificate") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("key_write_error_and_cleanup", func(t *testing.T) {
		// Valid path for cert, but invalid for key (parent dir is a regular file).
		certPath := filepath.Join(tmpDir, "valid-cert.pem")
		badKeyPath := filepath.Join(blockerFile, "key.pem")
		err := WriteSelfSigned(certPath, badKeyPath)
		if err == nil {
			t.Fatal("expected error writing to invalid key path, got nil")
		}
		if !strings.Contains(err.Error(), "write key") {
			t.Errorf("unexpected error message: %v", err)
		}
		// Verify that the cert file was cleaned up/deleted.
		if _, err := os.Stat(certPath); !os.IsNotExist(err) {
			t.Errorf("expected cert file to be removed, but got: %v", err)
		}
	})
}

type errorReader struct{}

func (errorReader) Read(p []byte) (n int, err error) {
	return 0, fmt.Errorf("mock random reader error")
}

func TestGenerateSelfSignedErrors(t *testing.T) {
	oldReader := rand.Reader
	t.Cleanup(func() {
		rand.Reader = oldReader
	})
	rand.Reader = errorReader{}

	_, _, err := generateSelfSigned()
	if err == nil {
		t.Fatal("expected error from generateSelfSigned when random reader fails, got nil")
	}
	if !strings.Contains(err.Error(), "generate ed25519 key") {
		t.Errorf("unexpected error: %v", err)
	}
}
