package loopback_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/app/sdk/loopback"
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
