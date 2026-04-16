package web

import (
	"html/template"
	"os"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// RenderContext holds all variables passed to every page template. Fields
// are exported so html/template can read them.
type RenderContext struct {
	// APIKey authenticates API requests from the frontend JS.
	APIKey string
	// NZBKey authenticates NZB-upload requests.
	NZBKey string
	// Version is the daemon version string, injected at handler-construction time.
	Version string

	// ActiveLang is the normalized BCP-47 language tag: underscores replaced
	// with hyphens, lowercased. Defaults to "en" when the config value is empty.
	ActiveLang string
	// RTL is true for right-to-left languages (Hebrew, Arabic, Farsi).
	// Kept as a small lookup because the full CLDR RTL list is overkill for
	// the three languages SABnzbd has historically shipped RTL support for.
	RTL bool

	// ColorScheme is the UI color scheme name. Empty string is treated as
	// "Light" by the template.
	// TODO: add to GeneralConfig when user-skin selection lands.
	ColorScheme string

	// Webdir is the URL path prefix for Glitter assets, not a filesystem path.
	// Hardcoded to "/static/glitter" for v1 because the embedded assets live there.
	Webdir string

	// NewRelease is the available new release string. Empty in v1; the
	// update-checker is not yet implemented.
	NewRelease string
	// NewRelURL is the URL for the new release announcement. Empty in v1.
	NewRelURL string

	// BytesPerSecList is recent per-second bandwidth samples for the UI graph.
	// Initialized to a non-nil empty slice so JSON/template renders "[]" not "null".
	BytesPerSecList []float64

	// HaveLogout is true when username-based auth is configured; shows a
	// logout button in the navigation menu.
	HaveLogout bool
	// HaveQuota is false in v1; PostProcConfig has no quota field yet.
	// TODO: derive from config once quota config lands.
	HaveQuota bool
	// HaveRSSDefined is true when at least one RSS feed is configured.
	HaveRSSDefined bool
	// HaveWatchedDir is true when a directory scanner path is configured.
	HaveWatchedDir bool

	// Pid is the daemon process ID, shown in the About dialog.
	Pid int
}

// rtlLanguages is the set of language tags for which text flows right-to-left.
// SABnzbd has shipped RTL UI support for these three; expand if new RTL
// translations land.
var rtlLanguages = map[string]bool{
	"he": true, // Hebrew
	"ar": true, // Arabic
	"fa": true, // Farsi / Persian
}

// BuildRenderContext constructs a fully populated RenderContext from a Config
// and a version string. It does not take a *queue.Queue because the minimal
// stub in Step 12.2 only needs version + apikey + config-derived bools.
// Queue-backed fields (e.g. speed samples) can be added in a later step.
func BuildRenderContext(cfg *config.Config, version string) RenderContext {
	var rc RenderContext

	cfg.WithRead(func(c *config.Config) {
		rc.APIKey = c.General.APIKey
		rc.NZBKey = c.General.NZBKey
		rc.HaveLogout = c.General.Username != ""
		rc.HaveRSSDefined = len(c.RSS) > 0
		rc.HaveWatchedDir = c.General.DirscanDir != ""

		lang := c.General.Language
		if lang == "" {
			lang = "en"
		}
		// Normalize: replace locale separator underscores with BCP-47 hyphens
		// and lowercase so the lang attribute is well-formed (e.g. "en_US" → "en-us").
		lang = strings.ToLower(strings.ReplaceAll(lang, "_", "-"))
		rc.ActiveLang = lang
		// RTL: check just the primary subtag (before any hyphen) against the
		// small known-RTL set. "he-IL" and "he" both map to RTL.
		primary := lang
		if idx := strings.IndexByte(lang, '-'); idx >= 0 {
			primary = lang[:idx]
		}
		rc.RTL = rtlLanguages[primary]
	})

	rc.Version = version
	rc.Webdir = "/static/glitter" // URL path, not a filesystem path
	rc.BytesPerSecList = []float64{}
	rc.Pid = os.Getpid()
	// ColorScheme defaults to "" (template treats as "Light").
	// NewRelease and NewRelURL are empty in v1; update-checker not implemented.

	return rc
}

// newFuncMap returns the html/template FuncMap that must be registered before
// any template referencing T or staticURL is parsed. Functions here are
// placeholders; Step 12.3 replaces T with a real i18n lookup.
func newFuncMap() template.FuncMap {
	return template.FuncMap{
		// T returns the translation key verbatim (English fallback).
		// Replaced with a real catalog lookup in Step 12.3.
		"T": func(key string) string { return key },
		// staticURL prepends the Glitter asset base path to a relative path.
		"staticURL": func(path string) string { return "/static/glitter/" + path },
	}
}
