package nntp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// buildTLSConfig returns a *tls.Config that implements the ssl_verify
// level requested by the server config. See spec §3.3.
//
//	Level 0 (None)     — skip all verification
//	Level 1 (Minimal)  — verify chain, ignore hostname
//	Level 2 (Hostname) — standard Go default: verify chain + hostname
//	Level 3 (Strict)   — same as level 2 plus extra Go-specific hardening
//
// Parity note: Python's levels map to OpenSSL flags (`CERT_NONE`,
// `X509_STRICT`, etc.). Go's crypto/tls already performs the strict
// extension checks that OpenSSL gates behind `X509_STRICT`, so levels
// 2 and 3 are effectively identical in behavior here. We still expose
// the distinction so the config surface matches the Python daemon
// verbatim and users migrating configs don't have to relabel servers.
//
// The returned config always enforces TLS 1.2+ per spec §3.3. A custom
// cipher string is applied via CipherSuites; because Go applies
// CipherSuites only to TLS 1.0-1.2, setting any ciphers caps the max
// version at 1.2 (matching the Python behavior documented in §3.3).
func buildTLSConfig(host string, verify config.SSLVerify, ciphers string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
	}

	if ciphers != "" {
		suites, err := parseCipherList(ciphers)
		if err != nil {
			return nil, fmt.Errorf("nntp: tls ciphers: %w", err)
		}
		cfg.CipherSuites = suites
		// Per spec §3.3: custom cipher list forces TLS 1.2 max because
		// Go's CipherSuites config is ignored under TLS 1.3.
		cfg.MaxVersion = tls.VersionTLS12
	}

	switch verify {
	case config.SSLVerifyNone:
		cfg.InsecureSkipVerify = true //nolint:gosec // level 0 is explicit user opt-in per ssl_verify=0
	case config.SSLVerifyMinimal:
		// Chain required, hostname skipped. We drive verification
		// manually via VerifyConnection (not VerifyPeerCertificate)
		// so the check still runs on resumed TLS sessions, and
		// disable Go's builtin verification which would otherwise
		// fail on hostname mismatch.
		cfg.InsecureSkipVerify = true //nolint:gosec // chain is still verified via VerifyConnection; hostname intentionally skipped per ssl_verify=1
		cfg.VerifyConnection = verifyConnectionIgnoreHostname
	case config.SSLVerifyHostname, config.SSLVerifyStrict:
		// Both rely on the default chain+hostname verification. The
		// "strict" level is retained for config parity with the Python
		// daemon; see package-level doc.
	default:
		return nil, fmt.Errorf("nntp: unknown ssl_verify level %d", verify)
	}

	return cfg, nil
}

// verifyConnectionIgnoreHostname validates the presented certificate
// chain against the system roots without enforcing a hostname match.
// Used for ssl_verify level 1. Runs on both fresh and resumed TLS
// sessions because it's hooked via VerifyConnection, not
// VerifyPeerCertificate.
func verifyConnectionIgnoreHostname(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("nntp: tls peer presented no certificates")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("nntp: load system roots: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}
	_, err = cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		// DNSName deliberately empty — hostname check skipped.
	})
	if err != nil {
		return fmt.Errorf("nntp: verify peer chain: %w", err)
	}
	return nil
}

// parseCipherList parses an OpenSSL-style cipher string into Go's
// tls.CipherSuite IDs. We accept the small set of IANA names that Go
// exposes via tls.CipherSuites(); entries we don't recognise are
// rejected rather than silently dropped so misconfigurations surface
// loudly.
//
// Input format: colon-separated names, e.g.
// "ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384".
// Both OpenSSL-style and IANA-style names are accepted (e.g.
// "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256").
func parseCipherList(s string) ([]uint16, error) {
	lookup := cipherNameIndex()
	var out []uint16
	start := 0
	for i := 0; i <= len(s); i++ {
		if i != len(s) && s[i] != ':' {
			continue
		}
		name := s[start:i]
		start = i + 1
		if name == "" {
			continue
		}
		id, ok := lookup[name]
		if !ok {
			return nil, fmt.Errorf("unknown cipher %q", name)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty cipher list")
	}
	return out, nil
}

// cipherNameIndex builds a name→id map once per call. Both the IANA
// name (as Go reports it) and the OpenSSL alias are accepted. The map
// is small (~dozens of entries) so rebuilding on each config reload
// is fine.
func cipherNameIndex() map[string]uint16 {
	idx := make(map[string]uint16)
	for _, cs := range tls.CipherSuites() {
		idx[cs.Name] = cs.ID
		if alias, ok := opensslCipherAlias[cs.Name]; ok {
			idx[alias] = cs.ID
		}
	}
	for _, cs := range tls.InsecureCipherSuites() {
		idx[cs.Name] = cs.ID
		if alias, ok := opensslCipherAlias[cs.Name]; ok {
			idx[alias] = cs.ID
		}
	}
	return idx
}

// opensslCipherAlias maps Go's IANA-style cipher names to the OpenSSL
// names users typically type in config files. Only the suites Go
// actually supports are covered — if a user requests a suite Go lacks,
// parseCipherList rejects the string.
var opensslCipherAlias = map[string]string{
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256": "ECDHE-ECDSA-AES128-GCM-SHA256",
	"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384": "ECDHE-ECDSA-AES256-GCM-SHA384",
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":   "ECDHE-RSA-AES128-GCM-SHA256",
	"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":   "ECDHE-RSA-AES256-GCM-SHA384",
	"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305":  "ECDHE-ECDSA-CHACHA20-POLY1305",
	"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305":    "ECDHE-RSA-CHACHA20-POLY1305",
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA":    "ECDHE-ECDSA-AES128-SHA",
	"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA":    "ECDHE-ECDSA-AES256-SHA",
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA":      "ECDHE-RSA-AES128-SHA",
	"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA":      "ECDHE-RSA-AES256-SHA",
	"TLS_RSA_WITH_AES_128_GCM_SHA256":         "AES128-GCM-SHA256",
	"TLS_RSA_WITH_AES_256_GCM_SHA384":         "AES256-GCM-SHA384",
	"TLS_RSA_WITH_AES_128_CBC_SHA":            "AES128-SHA",
	"TLS_RSA_WITH_AES_256_CBC_SHA":            "AES256-SHA",
}
