// Package page holds the chrome the status server's pages share: the document
// head, the header, the footer, and the stylesheet behind all three.
//
// It is a separate package from the app for the reason eumaeus's equivalent
// is: the moment there are two apps on this listener — a restore browser
// beside the status page — the second one written would otherwise carry its
// own copy of a <head> block and quietly differ from the first.
package page

import (
	"embed"
	"net/http"
)

// FS holds the chrome template, for an app to parse alongside its own pages:
//
//	template.New("layout.html").Funcs(funcs).
//		ParseFS(page.FS, "templates/chrome.html").
//		ParseFS(ownFS, "templates/layout.html", "templates/status.html")
//
// The definitions are page-head, page-top and page-foot. They are prefixed
// because a name like "head" in a shared set would collide with a page that
// defined its own.
//
//go:embed templates/chrome.html
var FS embed.FS

//go:embed static/style.css
var staticFS embed.FS

// StylePath is where the stylesheet is served.
const StylePath = "/style.css"

// Link is one entry in the header navigation.
type Link struct {
	Href    string
	Label   string
	Current bool
}

// Style serves the stylesheet.
//
// A handler rather than an http.FileServer over the embedded FS, because the
// Content-Security-Policy set in foundation/web forbids inline styles and
// permits exactly one stylesheet — so there is one file, and a file server
// would only add the ability to 404 on things that do not exist.
func Style() http.Handler {
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		// Embedded at compile time. A failure here is a build that should not
		// have been produced, and there is no sensible runtime recovery.
		panic("page: the stylesheet is missing from the binary: " + err.Error())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")

		// Overrides the no-store from foundation/web. The stylesheet is
		// compiled into the binary and holds nothing about anybody, and
		// re-fetching it on every poll of a page that refreshes itself is
		// waste.
		w.Header().Set("Cache-Control", "public, max-age=3600")

		w.Write(css)
	})
}
