package app

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
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

	// Verify CN is "sabnzbd"
	if cert.Subject.CommonName != "sabnzbd" {
		t.Errorf("CN = %q; want sabnzbd", cert.Subject.CommonName)
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

	// Verify key is RSA
	rsaKey, ok := privKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("private key type = %T; want *rsa.PrivateKey", privKey)
	}

	// Verify key is 4096 bits
	if rsaKey.N.BitLen() != 4096 {
		t.Errorf("key size = %d bits; want 4096", rsaKey.N.BitLen())
	}

	// Verify self-signed: signature should verify against public key
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}

	// Verify cert public key matches private key public key
	if !cert.PublicKey.(*rsa.PublicKey).Equal(rsaKey.Public()) {
		t.Error("certificate public key does not match private key")
	}
}

func TestGenerateSelfSignedUniqueness(t *testing.T) {
	t.Parallel()

	// Generate two certificates and verify they have different serial numbers
	cert1PEM, _, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}

	cert2PEM, _, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
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
	if cert.Subject.CommonName != "sabnzbd" {
		t.Errorf("CN from file = %q; want sabnzbd", cert.Subject.CommonName)
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

	// Verify key type
	if _, ok := privKey.(*rsa.PrivateKey); !ok {
		t.Fatalf("key type = %T; want *rsa.PrivateKey", privKey)
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
