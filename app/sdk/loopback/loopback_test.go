package loopback_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/app/sdk/loopback"
	"github.com/jroedel/sion-backup/foundation/web"
)

func guarded(t *testing.T) (*loopback.Guard, http.Handler) {
	t.Helper()

	g, err := loopback.New("127.0.0.1:7391")
	if err != nil {
		t.Fatal(err)
	}

	return g, g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("reached the handler"))
	}))
}

func TestAnOrdinaryGetIsAllowed(t *testing.T) {
	_, h := guarded(t)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7391/", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// TestDNSRebindingIsRefused is the attack that a form token does not stop: the
// attacker's page becomes same-origin with this server, and its script can
// read every response.
func TestDNSRebindingIsRefused(t *testing.T) {
	_, h := guarded(t)

	for _, host := range []string{
		"evil.example:7391",
		"backup.internal:7391",
		"127.0.0.1.nip.io:7391",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host

		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("Host %q reached the handler; a rebound page could read the configuration", host)
		}
	}
}

func TestTheRightHostsAreAllowed(t *testing.T) {
	_, h := guarded(t)

	for _, host := range []string{"127.0.0.1:7391", "localhost:7391", "[::1]:7391"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7391/", nil)
		req.Host = host

		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Host %q was refused with %d", host, rec.Code)
		}
	}
}

// TestTheWrongPortIsRefused: another program on this machine listening on
// 8080 is not this program, and a page it serves must not be treated as ours.
func TestTheWrongPortIsRefused(t *testing.T) {
	_, h := guarded(t)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil)
	req.Host = "127.0.0.1:8080"

	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("a request to the wrong port was accepted")
	}
}

// postForm builds a state-changing request the way a browser would.
func postForm(g *loopback.Guard, token string, headers map[string]string) *http.Request {
	form := url.Values{}
	if token != "" {
		form.Set(loopback.TokenField, token)
	}

	form.Set("paused", "on")

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7391/settings",
		strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:7391"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return req
}

func TestAGenuineFormPostIsAllowed(t *testing.T) {
	g, h := guarded(t)

	req := postForm(g, g.Token(), map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "http://127.0.0.1:7391",
	})

	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a real form submission was refused: %d %s", rec.Code, rec.Body)
	}
}

// TestCrossSitePostIsRefused is the classic attack: a page on the internet
// posting to localhost to repoint somebody's backups.
func TestCrossSitePostIsRefused(t *testing.T) {
	g, h := guarded(t)

	cases := map[string]*http.Request{
		"cross-site fetch metadata": postForm(g, g.Token(), map[string]string{
			"Sec-Fetch-Site": "cross-site",
			"Origin":         "https://evil.example",
		}),
		"foreign origin, no fetch metadata": postForm(g, g.Token(), map[string]string{
			"Origin": "https://evil.example",
		}),
		"no token at all": postForm(g, "", map[string]string{
			"Sec-Fetch-Site": "same-origin",
		}),
		"guessed token": postForm(g, "not-the-token", map[string]string{
			"Sec-Fetch-Site": "same-origin",
		}),
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403: %s", rec.Code, rec.Body)
			}
		})
	}
}

// TestATokenFromAPreviousProcessIsRefused. Two guards are two processes; a
// form rendered by the old one refers to a configuration that may be gone.
func TestATokenFromAPreviousProcessIsRefused(t *testing.T) {
	old, err := loopback.New("127.0.0.1:7391")
	if err != nil {
		t.Fatal(err)
	}

	current, h := guarded(t)

	if old.Token() == current.Token() {
		t.Fatal("two processes generated the same token")
	}

	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, postForm(current, old.Token(), map[string]string{"Sec-Fetch-Site": "same-origin"}))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", rec.Code)
	}
}

// TestACommandLineClientStillWorks: curl sends no Sec-Fetch-Site and no
// Origin, and must still be able to drive the server with a valid token. This
// is how the administrator scripts an install.
func TestACommandLineClientStillWorks(t *testing.T) {
	g, h := guarded(t)

	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, postForm(g, g.Token(), nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200: %s", rec.Code, rec.Body)
	}
}

// TestFirefoxCanSubmitTheForm is a regression test for a page that could not
// be used at all in one of the two browsers anybody has.
//
// The Fetch standard says that when a request's referrer policy is
// no-referrer, the Origin header of an unsafe request is serialised as "null"
// rather than as the page's real origin. Firefox implements that; Chrome does
// not. This server sent Referrer-Policy: no-referrer, so a POST from its own
// setup page arrived with Origin: null, was refused here, and the person who
// had just filled the whole page in got a bare error page reading "a request
// from null cannot change this machine's backup settings".
//
// Both halves are fixed and both are held here, composed the way daemon.go
// composes them, because either one alone leaves the page broken: the header
// so that the origin is stated, and the guard so that an opaque origin
// vouched for by Sec-Fetch-Site is not mistaken for another site's page.
func TestFirefoxCanSubmitTheForm(t *testing.T) {
	g, guarded := guarded(t)

	handler := web.SecureHeaders(guarded)

	// The header a page is served with, which is what decides the next
	// request's Origin.
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7391/setup", nil))

	if policy := get.Header().Get("Referrer-Policy"); policy == "no-referrer" {
		t.Error("the page is served with Referrer-Policy: no-referrer, which makes " +
			"Firefox send Origin: null on the form post and locks it out of every " +
			"form on this server")
	}

	// And the post that policy produced, in the version of Firefox that is
	// already running against a server that has not been restarted.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, postForm(g, g.Token(), map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "null",
	}))

	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200: %s", rec.Code, rec.Body)
	}
}

// TestAnOpaqueOriginIsNotAFreePass. Accepting "null" is a narrowing, not a
// hole: the other thing that produces one is a sandboxed iframe on somebody
// else's page, and what separates the two is the header the browser sends
// about where the request came from.
func TestAnOpaqueOriginIsNotAFreePass(t *testing.T) {
	for name, site := range map[string]string{
		"a sandboxed iframe on another site": "cross-site",
		"a page somewhere else on this host": "same-site",
		"a client that will not say":         "",
	} {
		t.Run(name, func(t *testing.T) {
			g, h := guarded(t)

			rec := httptest.NewRecorder()

			headers := map[string]string{"Origin": "null"}
			if site != "" {
				headers["Sec-Fetch-Site"] = site
			}

			h.ServeHTTP(rec, postForm(g, g.Token(), headers))

			if rec.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", rec.Code)
			}
		})
	}
}
