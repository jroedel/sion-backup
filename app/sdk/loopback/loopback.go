// Package loopback protects a configuration server that is bound to 127.0.0.1.
//
// # "It only listens on localhost" is not a security model
//
// It is a common and comfortable assumption, and on its own it is wrong in two
// specific ways that this package exists to close.
//
// **Cross-site request forgery.** Any web page the user visits can make their
// browser POST to http://127.0.0.1:7391/settings. The browser will do it
// happily; the request arrives from the loopback interface and looks exactly
// like the real thing. Without a check, any page on the internet can silently
// repoint somebody's backups at an attacker's bucket, or simply pause them.
//
// **DNS rebinding.** A page on evil.example can resolve its own hostname to
// 127.0.0.1 after the page has loaded. The browser then treats
// http://evil.example:7391 as same-origin with the attacker's script, and the
// script can *read* the responses — the whole configuration, including the
// repository URL and every directory on the machine. The same-origin policy
// does not help, because as far as the browser is concerned the origin is the
// attacker's.
//
// Both are defended here, and neither defence is optional:
//
//	Host                 must be a loopback name and the expected port
//	                     — closes DNS rebinding, because the browser sends
//	                       the attacker's hostname in Host
//	Sec-Fetch-Site       must be same-origin or none, on unsafe methods
//	Origin               must match, on unsafe methods
//	form token           must be present, on unsafe methods
//
// The token alone would be enough against classic CSRF, and is kept because
// Sec-Fetch-Site is not sent by every client and Origin is occasionally
// stripped by corporate middleboxes. Defence that degrades to "still safe" is
// worth more here than the tidiest single check.
//
// # There is no login
//
// Deliberately, and it is worth being explicit about why. Anybody who can
// reach 127.0.0.1 on this machine is already logged in as this user; they can
// read the machine token, the database and the binary. A password would protect
// nothing and would guarantee a sticky note on the monitor.
package loopback

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// TokenField is the form field the token travels in.
const TokenField = "form_token"

// Guard enforces everything in the package comment.
type Guard struct {
	// port is the port this server is expected to be reached on. Empty means
	// any, which is what a test wants and a deployment does not.
	port string

	// token is generated per process. It is not a session token and does not
	// need to be: the only thing it proves is that the request came from a
	// page this server rendered, and a page rendered by the previous process
	// belongs to a configuration that may no longer exist.
	token string
}

// New builds a guard for a server listening on addr.
func New(addr string) (*Guard, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("loopback: %q is not a host:port address: %w", addr, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("loopback: generating the form token: %w", err)
	}

	return &Guard{port: port, token: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// Token is the value to render into every form.
func (g *Guard) Token() string { return g.token }

// Wrap applies the guard to a handler.
func (g *Guard) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			// A deliberately unhelpful message. Whoever is reading it is
			// either the administrator, who has the source, or an attacker's
			// script, which should learn nothing.
			http.Error(w, "this page is only reachable at http://127.0.0.1:"+g.port+"/",
				http.StatusMisdirectedRequest)

			return
		}

		if safe(r.Method) {
			h.ServeHTTP(w, r)

			return
		}

		if err := g.checkUnsafe(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)

			return
		}

		h.ServeHTTP(w, r)
	})
}

// safe reports whether a method may change nothing.
func safe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// hostAllowed is the DNS rebinding defence.
//
// The browser sends whatever hostname was in the address bar, so a page served
// from evil.example resolving to 127.0.0.1 arrives with Host: evil.example and
// is refused here — before any handler has rendered anything for its script to
// read.
func (g *Guard) hostAllowed(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port in Host. That cannot be this server, which is never on 80.
		name, port = host, ""
	}

	if g.port != "" && port != g.port {
		return false
	}

	switch strings.ToLower(name) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// checkUnsafe applies the three CSRF defences to a state-changing request.
func (g *Guard) checkUnsafe(r *http.Request) error {
	// Sec-Fetch-Site is sent by every current browser and by nothing else.
	// "none" means the user typed the URL or used a bookmark; "same-origin"
	// means a page from this server. Anything else is another site's page
	// talking to us, which is the attack.
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "none", "same-origin":
		// Empty is allowed: curl and older clients send nothing, and the token
		// below still has to be right.
	default:
		return fmt.Errorf("a request from %s cannot change this machine's backup settings", site)
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !g.hostAllowed(u.Host) {
			return fmt.Errorf("a request from %s cannot change this machine's backup settings", origin)
		}
	}

	// ParseForm reads the body, which every handler below then reads from
	// r.PostForm rather than the body again.
	if err := r.ParseForm(); err != nil {
		return fmt.Errorf("the form could not be read: %w", err)
	}

	// Constant time, out of habit rather than necessity: the token is not a
	// long-lived secret, but comparing secrets in constant time is not a habit
	// worth having exceptions to.
	got := r.PostFormValue(TokenField)
	if subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) != 1 {
		return fmt.Errorf("this form is from an older session of the backup agent; " +
			"reload the page and try again")
	}

	return nil
}
