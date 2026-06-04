package nntp

import (
	"crypto/tls"
	"testing"
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
