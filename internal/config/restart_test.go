package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// unsetEnvVar names an environment variable the tests below set to "", so a
// path that references it expands to nothing.
const unsetEnvVar = "GONZBD_RESTART_TEST_UNSET"

// requireSurvivesRestart saves cfg, starts a fresh Load from the file the way
// the daemon does at startup, and requires that the load succeeds, that the
// log level startup parses is valid, and that the loaded config is the one
// that was saved.
func requireSurvivesRestart(t *testing.T, cfg *Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gonzbd.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Set accepted a value the next start refuses: Load: %v", err)
	}
	// cmd/gonzbd's configureLogging parses these before anything else runs.
	if _, err := loaded.General.ParseLogLevel(); err != nil {
		t.Fatalf("Set accepted a value the next start refuses: ParseLogLevel: %v", err)
	}
	if _, err := loaded.General.ParseLogLevels(); err != nil {
		t.Fatalf("Set accepted a value the next start refuses: ParseLogLevels: %v", err)
	}
	live := marshalForCompare(t, cfg.Snapshot())
	if got := marshalForCompare(t, loaded); !bytes.Equal(live, got) {
		t.Fatalf("the restarted config differs from the live one\nlive:\n%s\nafter restart:\n%s", live, got)
	}
}

// TestSet_ThenRestart_Issue899 reproduces the two reported cases: a value
// set_config accepted and saved, which the next start then refused.
func TestSet_ThenRestart_Issue899(t *testing.T) {
	t.Setenv(unsetEnvVar, "")
	cases := []struct{ section, keyword, value string }{
		{"general", "log_level", "bogus"},
		{"general", "complete_dir", "$" + unsetEnvVar},
	}
	for _, tc := range cases {
		t.Run(tc.keyword+"="+tc.value, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			if err := cfg.Set(tc.section, tc.keyword, tc.value); err != nil {
				// Refused: the live config and the file keep the old value.
				if verr := cfg.Validate(); verr != nil {
					t.Fatalf("a refused Set left the live config invalid: %v", verr)
				}
				return
			}
			requireSurvivesRestart(t, cfg)
		})
	}
}

// restartCandidates returns the raw set_config values tried for a field of
// type ft: valid, boundary and invalid values, including paths that expand.
func restartCandidates(ft reflect.Type) []string {
	switch {
	case ft == reflect.TypeFor[ByteSize]():
		return []string{"0", "1M", "-1"}
	case ft == reflect.TypeFor[Percent]():
		return []string{"0", "50", "100"}
	}
	switch ft.Kind() {
	case reflect.String:
		return []string{
			"", "x", "bogus", "info", "-mlp", "755", "/", "..", "~/x",
			"$" + unsetEnvVar, "$" + unsetEnvVar + "/sub",
		}
	case reflect.Int, reflect.Int64:
		return []string{"-5", "-1", "0", "1", "2", "99"}
	case reflect.Bool:
		return []string{"true", "false"}
	case reflect.Slice:
		return []string{`[]`, `["x"]`, `["("]`, `["exe"]`, `["10.0.0.0/8"]`}
	}
	return []string{"0", "x"}
}

// TestSet_EveryAcceptedValueSurvivesRestart is the property behind #899: for
// every settable key, a value Set accepts must load on the next start and
// come back unchanged.
func TestSet_EveryAcceptedValueSurvivesRestart(t *testing.T) {
	t.Setenv(unsetEnvVar, "")
	attempts := setAttempts()
	accepted := 0
	for _, a := range attempts {
		t.Run(a.section+"/"+a.keyword+"="+a.value, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			if err := cfg.Set(a.section, a.keyword, a.value); err != nil {
				return
			}
			accepted++
			requireSurvivesRestart(t, cfg)
		})
	}
	// Guard against a harness that silently stops exercising Set.
	if accepted < len(attempts)/4 {
		t.Fatalf("Set accepted only %d of %d attempts; the candidates no longer exercise it", accepted, len(attempts))
	}
}

// TestLoadAndSetAgree pins the converse direction: the same raw value, given
// to set_config or written into the YAML file, is refused by both or accepted
// by both, and accepted it yields the same config.
func TestLoadAndSetAgree(t *testing.T) {
	t.Setenv(unsetEnvVar, "")
	for _, a := range setAttempts() {
		t.Run(a.section+"/"+a.keyword+"="+a.value, func(t *testing.T) {
			if (a.keyword == "api_key" || a.keyword == "nzb_key") && a.value == "" {
				// By design: Load generates a key the file leaves empty
				// (generateMissingKeys); set_config refuses an empty one.
				t.Skip("an empty key is generated at load and refused by Set")
			}
			base, err := Default()
			if err != nil {
				t.Fatalf("Default: %v", err)
			}

			viaSet := base.Snapshot()
			setErr := viaSet.Set(a.section, a.keyword, a.value)

			// The same raw value written into the file, as an operator
			// editing the YAML would.
			raw := base.Snapshot()
			if err := raw.assign(a.section, a.keyword, a.value); err != nil {
				if setErr == nil {
					t.Fatalf("Set accepted a value assign cannot write: %v", err)
				}
				return
			}
			path := filepath.Join(t.TempDir(), "gonzbd.yaml")
			if err := raw.Save(path); err != nil {
				t.Fatalf("Save: %v", err)
			}
			viaLoad, loadErr := Load(path)

			switch {
			case setErr != nil && loadErr != nil:
			case setErr != nil:
				t.Fatalf("Load accepts what Set refuses: %v", setErr)
			case loadErr != nil:
				t.Fatalf("Set accepts what Load refuses: %v", loadErr)
			default:
				if s, l := marshalForCompare(t, viaSet), marshalForCompare(t, viaLoad); !bytes.Equal(s, l) {
					t.Fatalf("Set and Load disagree\nSet:\n%s\nLoad:\n%s", s, l)
				}
			}
		})
	}
}

type setAttempt struct{ section, keyword, value string }

// setAttempts lists the raw set_config inputs both properties above are
// checked over: restartCandidates for every field of the flat sections, and
// server and category arrays around their defaulted and range-checked keys.
func setAttempts() []setAttempt {
	var attempts []setAttempt
	for _, section := range []string{"general", "downloads", "postproc"} {
		st := findFieldByTag(reflect.ValueOf(&Config{}).Elem(), section).Type()
		for f := range st.Fields() {
			for _, v := range restartCandidates(f.Type) {
				attempts = append(attempts, setAttempt{section, tagName(f), v})
			}
		}
	}
	server := `{"name":"s","host":"news.example.com","connections":1,"timeout":30,"pipelining_requests":2,"enable":true%s}`
	for _, extra := range []string{``, `,"port":0`, `,"port":0,"ssl":true`, `,"port":563,"ssl":true,"ssl_verify":0`} {
		attempts = append(attempts, setAttempt{"servers", "", "[" + fmt.Sprintf(server, extra) + "]"})
	}
	for _, prio := range []int{-100, -4, -2, -1, 0, 2, 3, 99} {
		attempts = append(attempts, setAttempt{"categories", "",
			fmt.Sprintf(`[{"name":"Default","pp":3,"script":"None","priority":%d}]`, prio)})
	}
	return attempts
}

// tagName is the json (else yaml) tag name findFieldByTag matches on.
func tagName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		tag = f.Tag.Get("yaml")
	}
	for i := range len(tag) {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}
