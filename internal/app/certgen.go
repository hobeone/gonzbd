package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateSelfSigned generates a 4096-bit RSA key and self-signed certificate
// suitable for HTTPS. It returns the PEM-encoded certificate and key as byte slices.
//
// The certificate has CN=sabnzbd and SANs for 127.0.0.1, ::1, and localhost.
// Validity is from now-1h (for clock skew) to now+5 years. SerialNumber is a
// random 128-bit value.
func GenerateSelfSigned() (certPEM, keyPEM []byte, err error) {
	// Generate RSA key
	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, fmt.Errorf("generate RSA key: %w", err)
	}

	// Generate random serial number
	serialNum, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial number: %w", err)
	}

	// Create certificate template
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			CommonName: "sabnzbd",
		},
		NotBefore: now.Add(-time.Hour), //nolint:gosec // intentional: clock skew mitigation
		NotAfter:  now.AddDate(5, 0, 0),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames: []string{
			"localhost",
		},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}

	// Create self-signed certificate
	certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	// Encode certificate to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})

	// Encode private key to PEM
	privKeyBytes, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privKeyBytes,
	})

	return certPEM, keyPEM, nil
}

// WriteSelfSigned generates a self-signed certificate and writes it to files.
// certPath is written with permissions 0o644; keyPath is written with permissions 0o600.
// Writes are atomic (temp file + rename).
func WriteSelfSigned(certPath, keyPath string) error {
	certPEM, keyPEM, err := GenerateSelfSigned()
	if err != nil {
		return err
	}

	// Write certificate atomically
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write certificate: %w", err)
	}

	// Write key atomically
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		// Try to clean up the cert file if key write fails
		//nolint:errcheck // best-effort cleanup; key write error takes precedence
		_ = os.Remove(certPath)
		return fmt.Errorf("write key: %w", err)
	}

	return nil
}

// writeFileAtomic writes data to a file atomically by writing to a temp file
// in the same directory and renaming it.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".cert-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		//nolint:errcheck // best-effort cleanup; write error takes precedence
		_ = tmp.Close()
		//nolint:errcheck // best-effort cleanup; write error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("write: %w", err)
	}

	if err := tmp.Close(); err != nil {
		//nolint:errcheck // best-effort cleanup; close error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("close: %w", err)
	}

	if err := os.Chmod(tmpName, perm); err != nil {
		//nolint:errcheck // best-effort cleanup; chmod error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		//nolint:errcheck // best-effort cleanup; rename error takes precedence
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
