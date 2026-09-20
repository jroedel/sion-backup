// Package page holds the chrome the status server's pages share: the document
// head, the header, the footer, and the stylesheet behind all three.
//
// It is a separate package from the app for the reason eumaeus's equivalent
// is: the moment there are two apps on this listener — a restore browser
// beside the status page — the second one written would otherwise carry its
// own copy of a <head> block and quietly differ from the first.
package page

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

// styleHref is StylePath with a fingerprint of the file's own contents.
//
// # Why the URL carries a version
//
// The stylesheet used to be served as /style.css with `max-age=3600` and
// nothing else: no ETag, no Last-Modified, no version. A browser that had it
// did not ask again for an hour, so for an hour after an upgrade the new
// program served its pages with the old program's stylesheet -- and a fix to
// the layout looked like a fix that had not shipped. That is exactly how it
// was found: a machine on the new build, rendering a page whose spacing had
// been corrected two releases earlier, with the correction nowhere in sight.
//
// The fingerprint is of the contents rather than the build version, because a
// developer's build reports "dev" for as long as they are working on it, and
// that is the one place where the stylesheet changes most often.
var styleHref = func() string {
	sum := sha256.Sum256(mustStyle())

	return StylePath + "?v=" + hex.EncodeToString(sum[:])[:12]
}()

// StyleHref is what a page's <link> should point at.
func StyleHref() string { return styleHref }

// mustStyle reads the embedded stylesheet or refuses to start.
//
// Embedded at compile time: a failure here is a build that should not have
// been produced, and there is no sensible runtime recovery.
func mustStyle() []byte {
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		panic("page: the stylesheet is missing from the binary: " + err.Error())
	}

	return css
}

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
	css := mustStyle()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")

		// Overrides the no-store from foundation/web. The stylesheet is
		// compiled into the binary and holds nothing about anybody, and
		// re-fetching it on every poll of a page that refreshes itself is
		// waste.
		//
		// Cached hard and for ever, which is safe only because the href
		// carries a fingerprint of these bytes: a changed stylesheet is a
		// changed URL, and the old answer can never be served for the new
		// one. See styleHref for the upgrade this got wrong.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

		w.Write(css)
	})
}
