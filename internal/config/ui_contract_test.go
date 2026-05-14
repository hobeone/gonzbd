package config

import (
	"reflect"
	"strings"
	"testing"
)

// uiKeywords is the canonical list of (section, keyword) pairs that Svelte
// config components send to the API via mode=set_config.
//
// HOW TO MAINTAIN THIS LIST
// ─────────────────────────
// Add an entry here when you add a new keyword= prop to any ConfigInput,
// ConfigSwitch, or ConfigTextarea component. Remove it when you remove the
// component. The test verifies that every entry resolves to a real Go struct
// field via its json tag — if the keyword doesn't exist in Go, Set() returns
// "invalid keyword" and the test fails.
//
// Sections use their internal names (e.g. "general", not "misc").
// The API's "general"↔"misc" alias is transparent to this test.
var uiKeywords = []struct {
	section string
	keyword string
	// component identifies the Svelte file for grep/blame purposes.
	component string
}{
	// ── GeneralSection.svelte ────────────────────────────────────────────────
	{"general", "host", "GeneralSection.svelte"},
	{"general", "port", "GeneralSection.svelte"},
	{"general", "api_key", "GeneralSection.svelte"},
	{"general", "nzb_key", "GeneralSection.svelte"},
	{"general", "download_dir", "GeneralSection.svelte"},
	{"general", "complete_dir", "GeneralSection.svelte"},
	{"general", "log_level", "GeneralSection.svelte"},

	// ── DownloadsSection.svelte ──────────────────────────────────────────────
	{"downloads", "bandwidth_max", "DownloadsSection.svelte"},
	{"downloads", "min_free_space", "DownloadsSection.svelte"},
	{"downloads", "write_cache_size", "DownloadsSection.svelte"},
	{"downloads", "max_art_tries", "DownloadsSection.svelte"},
	{"downloads", "top_only", "DownloadsSection.svelte"},
	{"downloads", "pre_check", "DownloadsSection.svelte"},
	{"downloads", "replace_illegal_with", "DownloadsSection.svelte"},
	{"downloads", "replace_spaces_with", "DownloadsSection.svelte"},
	{"downloads", "strip_diacritics", "DownloadsSection.svelte"},
	{"downloads", "cleanup_list", "DownloadsSection.svelte"},

	// ── PostProcSection.svelte ───────────────────────────────────────────────
	// Extraction
	{"postproc", "enable_unrar", "PostProcSection.svelte"},
	{"postproc", "enable_7zip", "PostProcSection.svelte"},
	{"postproc", "prefer_7zip", "PostProcSection.svelte"},
	{"postproc", "enable_filejoin", "PostProcSection.svelte"},
	{"postproc", "enable_tsjoin", "PostProcSection.svelte"},
	{"postproc", "enable_recursive", "PostProcSection.svelte"},
	{"postproc", "direct_unpack", "PostProcSection.svelte"},
	{"postproc", "flat_unpack", "PostProcSection.svelte"},
	{"postproc", "overwrite_files", "PostProcSection.svelte"},
	{"postproc", "ignore_unrar_dates", "PostProcSection.svelte"},
	// Par2 Repair
	{"postproc", "par2_turbo", "PostProcSection.svelte"},
	{"postproc", "enable_all_par", "PostProcSection.svelte"},
	{"postproc", "process_unpacked_par2", "PostProcSection.svelte"},
	// Cleanup & Output
	{"postproc", "enable_par_cleanup", "PostProcSection.svelte"},
	{"postproc", "enable_rar_cleanup", "PostProcSection.svelte"},
	{"postproc", "deobfuscate_filenames", "PostProcSection.svelte"},
	{"postproc", "ignore_samples", "PostProcSection.svelte"},
	{"postproc", "folder_rename", "PostProcSection.svelte"},
	{"postproc", "cleanup_extensions", "PostProcSection.svelte"},
	{"postproc", "permissions", "PostProcSection.svelte"},
	// External Tools
	{"postproc", "par2_command", "PostProcSection.svelte"},
	{"postproc", "unrar_command", "PostProcSection.svelte"},
	{"postproc", "sevenz_command", "PostProcSection.svelte"},
	{"postproc", "password_file", "PostProcSection.svelte"},
	// Advanced
	{"postproc", "script_can_fail", "PostProcSection.svelte"},
	{"postproc", "nice", "PostProcSection.svelte"},
	{"postproc", "ionice", "PostProcSection.svelte"},
	{"postproc", "extra_unrar_params", "PostProcSection.svelte"},
	{"postproc", "extra_par2_params", "PostProcSection.svelte"},
}

// TestUIKeywordsAreValidConfigTags verifies that every (section, keyword)
// pair in uiKeywords resolves to a real Go struct field via its json tag.
//
// This test calls config.Set with an empty string value (the same code path
// the live API uses). An "invalid keyword" error means the keyword in the
// Svelte component has no backing field in Go — that setting silently fails
// in production when the user clicks Save.
//
// Type-conversion errors (e.g. empty string → bool) are expected and are NOT
// treated as failures: they prove the field exists and is reachable.
func TestUIKeywordsAreValidConfigTags(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	for _, kw := range uiKeywords {
		t.Run(kw.section+"/"+kw.keyword, func(t *testing.T) {
			setErr := cfg.Set(kw.section, kw.keyword, "")
			if setErr != nil && strings.Contains(setErr.Error(), "invalid keyword") {
				t.Errorf(
					"[%s] keyword %q in section %q has no matching json tag in any Go config struct\n"+
						"  → Either the Svelte keyword= prop is misspelled, or the Go field was renamed/removed.",
					kw.component, kw.keyword, kw.section,
				)
			}
			// Any other error (type conversion, validation) is fine — it proves
			// the field exists and was reached by the reflection router.
		})
	}
}

// TestAllFlatConfigTagsAreSettable verifies the inverse direction: every json
// tag on every flat config struct (General, Downloads, PostProc) can be
// reached by config.Set(). This catches Go fields that are serialised into the
// JSON response but whose json tag does not round-trip through Set().
//
// Unlike TestUIKeywordsAreValidConfigTags (which checks Svelte→Go), this test
// checks Go→Go: it enumerates tags automatically from the struct so it never
// needs updating when new fields are added to the Go side.
func TestAllFlatConfigTagsAreSettable(t *testing.T) {
	type sectionSpec struct {
		name  string
		value any
	}

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	sections := []sectionSpec{
		{"general", cfg.General},
		{"downloads", cfg.Downloads},
		{"postproc", cfg.PostProc},
	}

	for _, sec := range sections {
		rv := reflect.TypeOf(sec.value)
		for field := range rv.Fields() {
			tag := field.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			keyword := strings.Split(tag, ",")[0]
			if keyword == "" || keyword == "-" {
				continue
			}

			t.Run(sec.name+"/"+keyword, func(t *testing.T) {
				setErr := cfg.Set(sec.name, keyword, "")
				if setErr != nil && strings.Contains(setErr.Error(), "invalid keyword") {
					t.Errorf(
						"Go field %s.%s (json:%q) cannot be reached via Set(%q, %q, \"\").\n"+
							"  → The json tag exists on the struct but findFieldByTag() did not find it.",
						rv.Name(), field.Name, keyword, sec.name, keyword,
					)
				}
			})
		}
	}
}
