package nntp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestParseCipherListDirect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "empty list",
			input:   "",
			wantErr: true,
		},
		{
			name:    "only colon",
			input:   ":",
			wantErr: true,
		},
		{
			name:    "single valid IANA",
			input:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			wantErr: false,
		},
		{
			name:    "single valid OpenSSL",
			input:   "ECDHE-RSA-AES128-GCM-SHA256",
			wantErr: false,
		},
		{
			name:    "multiple valid mixed",
			input:   "ECDHE-ECDSA-AES256-GCM-SHA384:TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			wantErr: false,
		},
		{
			name:    "invalid cipher name",
			input:   "INVALID-CIPHER-NAME",
			wantErr: true,
		},
		{
			name:    "valid and invalid mixed",
			input:   "ECDHE-RSA-AES128-GCM-SHA256:INVALID-CIPHER",
			wantErr: true,
		},
		{
			name:    "trailing colons",
			input:   "ECDHE-RSA-AES128-GCM-SHA256:",
			wantErr: false,
		},
		{
			name:    "leading colons",
			input:   ":ECDHE-RSA-AES128-GCM-SHA256",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCipherList(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseCipherList(%q) error = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if err == nil && len(got) == 0 {
				t.Error("expected non-empty cipher list for successful parse")
			}
		})
	}
}

func TestCipherNameIndexDirect(t *testing.T) {
	t.Parallel()
	idx := cipherNameIndex()
	if len(idx) == 0 {
		t.Fatal("cipherNameIndex returned empty map")
	}

	// Verify standard expected IANA and OpenSSL mappings.
	checks := []string{
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"ECDHE-RSA-AES128-GCM-SHA256",
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"ECDHE-ECDSA-AES128-GCM-SHA256",
	}

	for _, c := range checks {
		if _, ok := idx[c]; !ok {
			t.Errorf("cipherNameIndex missing expected mapping for %q (available keys: %v)", c, keysOf(idx))
		}
	}
}

func keysOf(m map[string]uint16) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestVerifyConnectionIgnoreHostnameDirect(t *testing.T) {
	t.Parallel()
	t.Run("no peer certificates", func(t *testing.T) {
		cs := tls.ConnectionState{
			PeerCertificates: nil,
		}
		err := verifyConnectionIgnoreHostname(cs)
		if err == nil {
			t.Error("expected error when no peer certificates presented")
		}
	})
}

func TestVerifyConnectionIgnoreHostname_ManualVerify(t *testing.T) {
	t.Parallel()

	// Generate a dummy peer certificate
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Inc"},
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(1 * time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	cs := tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
	}

	err = verifyConnectionIgnoreHostname(cs)
	if err == nil {
		t.Fatal("expected verification failure, got nil")
	}

	// Verify that the error is not "load system roots" (tls.go:80:9 negation)
	if strings.Contains(err.Error(), "load system roots") {
		t.Errorf("got error with 'load system roots', but expected 'verify peer chain': %v", err)
	}

	// Verify that the error is "verify peer chain" (tls.go:92:9 negation)
	if !strings.Contains(err.Error(), "verify peer chain") {
		t.Errorf("expected error to contain 'verify peer chain', got: %v", err)
	}
}

// TestBuildTLSConfig_LoadedServerWithoutSSLVerifyVerifies loads a config file
// whose server entry leaves out ssl_verify and requires the TLS config built
// from it to verify the peer: an omitted key must not mean level 0.
func TestBuildTLSConfig_LoadedServerWithoutSSLVerifyVerifies(t *testing.T) {
	t.Parallel()
	base, err := config.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	path := filepath.Join(t.TempDir(), "gonzbd.yaml")
	if err := base.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const server = "servers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n    ssl: true\n    pipelining_requests: 2\n    timeout: 60\n    enable: true\n"
	withServer := strings.Replace(string(b), "servers: []\n", server, 1)
	if withServer == string(b) {
		t.Fatal(`"servers: []" not found in the saved default config; fixture assumption is stale`)
	}
	if err := os.WriteFile(path, []byte(withServer), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	srv := cfg.Servers[0]
	tc, err := buildTLSConfig(srv.Host, srv.SSLVerify, srv.SSLCiphers)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tc.InsecureSkipVerify {
		t.Errorf("InsecureSkipVerify = true for a server that omits ssl_verify (level %d), want chain and hostname verification", srv.SSLVerify)
	}
}
