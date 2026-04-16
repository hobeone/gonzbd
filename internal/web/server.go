// Package web serves the SABnzbd-Go web interface and static Glitter
// assets. Static assets are embedded at build time via //go:embed so
// the binary is self-contained.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/hobeone/sabnzbd-go/internal/i18n"
)

//go:embed static
var staticFS embed.FS

//go:embed templates
var templatesFS embed.FS

// Handler returns an http.Handler serving the web UI and static assets
// using a zero-value RenderContext. Kept for backward compatibility with
// tests that do not supply a render context.
//
// Routes:
//   - GET /              → main.html.tmpl rendered with a zero RenderContext
//   - GET /static/...    → Glitter assets under static/glitter/
//   - GET /staticcfg/... → shared icons (favicons, apple-touch-icons) under static/staticcfg/.
//     Mounted at /staticcfg/ rather than /static/staticcfg/ because upstream
//     SABnzbd's main.tmpl references these via "./staticcfg/ico/..." paths.
//
// The handler is stateless and safe to serve concurrently.
func Handler() http.Handler {
	return HandlerWithContext(RenderContext{})
}

// HandlerWithContext returns an http.Handler that renders main.html.tmpl with
// the supplied RenderContext. The template is parsed once at call time; a
// parse error panics because it indicates a broken build (the template is
// embedded at compile time and must be well-formed).
func HandlerWithContext(rc RenderContext) http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Can only happen with a broken build — the embed directive guarantees
		// "static" exists at compile time.
		panic("web: embed subtree 'static' missing: " + err.Error())
	}

	staticcfgSub, err := fs.Sub(staticFS, "static/staticcfg")
	if err != nil {
		panic("web: embed subtree 'static/staticcfg' missing: " + err.Error())
	}

	// Parse all templates from the templates directory using ParseFS.
	// This loads main.html.tmpl and all partial templates (include_messages.html.tmpl, etc.).
	// Use the FuncMap so T/staticURL calls inside the templates resolve at parse time.
	// Load the default English catalog (ported from upstream skintext.SKIN_TEXT)
	// so translation keys resolve to English text rather than raw keys.
	tmpl, err := template.New("main.html.tmpl").Funcs(newFuncMap(i18n.DefaultEnglish())).ParseFS(templatesFS, "templates/*.html.tmpl")
	if err != nil {
		panic("web: template parse error: " + err.Error())
	}

	mux := http.NewServeMux()

	// Serve /static/ prefix — strip the prefix then serve from the embed FS.
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Serve /staticcfg/ prefix from the staticcfg subtree (matches upstream URL layout).
	mux.Handle("/staticcfg/", http.StripPrefix("/staticcfg/", http.FileServer(http.FS(staticcfgSub))))

	// Render main.html.tmpl at "/".
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, rc); err != nil {
			// Template execute errors after headers are sent cannot be recovered;
			// log the error but do not write a second status code.
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	})

	return mux
}
