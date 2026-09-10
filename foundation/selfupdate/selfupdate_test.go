package selfupdate_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// TestNewerRefusesToReplaceABuildItCannotReasonAbout is the rule that keeps a
// developer's working copy safe: `go build` stamps "dev", and a machine that
// treated that as "older than v1.0.0" would helpfully delete the change
// somebody was in the middle of testing.
func TestNewerRefusesToReplaceABuildItCannotReasonAbout(t *testing.T) {
	for _, c := range []struct {
		current, candidate string
		want               bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.3.0", true},
		{"v1.2.3", "v2.0.0", true},
		{"1.2.3", "1.2.4", true},

		{"v1.2.3", "v1.2.3", false},
		{"v1.2.4", "v1.2.3", false},
		{"v2.0.0", "v1.9.9", false},

		// Neither side may be a build this cannot compare.
		{"dev", "v1.2.3", false},
		{"v1.2.3", "dev", false},
		{"abc1234", "v1.2.3", false},
		{"v1.2.3-4-gabc1234", "v1.2.4", false},
		{"v1.2.3", "v1.2.4-rc1", false},
		{"v1.2", "v1.3", false},
	} {
		if got := selfupdate.Newer(c.current, c.candidate); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetNameMatchesWhatTheMakefileBuilds(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "sion-backup-linux-amd64"},
		{"darwin", "arm64", "sion-backup-darwin-arm64"},
		{"windows", "amd64", "sion-backup-windows-amd64.exe"},
	} {
		if got := selfupdate.AssetName(c.goos, c.goarch); got != c.want {
			t.Errorf("AssetName(%s, %s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// fakeBinary writes a shell script that behaves enough like the program to be
// smoke-tested: it prints a version when asked.
func fakeBinary(t *testing.T, path, version string) []byte {
	t.Helper()

	body := "#!/bin/sh\necho \"sion-backup " + version + " (test)\"\n"

	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	return []byte(body)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

// source is a Source that answers with whatever it is given.
type source struct {
	release selfupdate.Release
	err     error
}

func (s source) Latest(context.Context, string, string) (selfupdate.Release, error) {
	return s.release, s.err
}

// serve hands out one file.
func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))

	t.Cleanup(srv.Close)

	return srv
}

// TestAnUpdateIsInstalledAndTheOldOneKept walks the whole path on a fake
// binary: hash, smoke test, swap, and the .old left behind for the next start
// to remove.
func TestAnUpdateIsInstalledAndTheOldOneKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	fakeBinary(t, exe, "v1.0.0")

	next := filepath.Join(dir, "next")
	body := fakeBinary(t, next, "v1.1.0")
	srv := serve(t, body)

	u, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: srv.URL, SHA256: sum(body),
		}},
		Current:    "v1.0.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	release, ok, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !ok || release.Version != "v1.1.0" {
		t.Fatalf("applied=%v release=%+v", ok, release)
	}

	installed, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(installed), "v1.1.0") {
		t.Error("the new binary is not the one in place")
	}

	if info, err := os.Stat(exe); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the installed binary is not executable: %v %v", info, err)
	}

	// The previous version is still there until the next start removes it,
	// which is the only rollback this has.
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Errorf("the previous binary was not kept: %v", err)
	}

	u.CleanupOld()

	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Error("CleanupOld left the previous binary behind")
	}
}

// TestADownloadThatDoesNotMatchItsHashIsNeverRun is the one that matters.
// This binary reads every file on the machine and holds the credentials to
// the off-site copy.
func TestADownloadThatDoesNotMatchItsHashIsNeverRun(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	fakeBinary(t, exe, "v1.0.0")

	srv := serve(t, []byte("#!/bin/sh\necho pwned\n"))

	u, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: srv.URL, SHA256: sum([]byte("something else entirely")),
		}},
		Current:    "v1.0.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, ok, err := u.Apply(context.Background())

	if ok || err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("applied=%v err=%v, want a refusal", ok, err)
	}

	// Nothing was moved and nothing was left lying around.
	current, _ := os.ReadFile(exe)
	if !strings.Contains(string(current), "v1.0.0") {
		t.Error("the running binary was replaced by one that failed verification")
	}

	for _, leftover := range []string{exe + ".new", exe + ".old"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s was left behind", filepath.Base(leftover))
		}
	}
}

// TestABinaryThatDoesNotRunIsNotInstalled covers the truncated download and
// the build for the wrong architecture — neither of which a hash can catch,
// because both hash correctly to the wrong thing.
func TestABinaryThatDoesNotRunIsNotInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	fakeBinary(t, exe, "v1.0.0")

	// Hashes correctly. Says the wrong thing when run.
	body := []byte("#!/bin/sh\necho \"sion-backup v0.0.9\"\n")
	srv := serve(t, body)

	u, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: srv.URL, SHA256: sum(body),
		}},
		Current:    "v1.0.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, ok, err := u.Apply(context.Background())

	if ok || err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("applied=%v err=%v, want the smoke test to refuse it", ok, err)
	}

	current, _ := os.ReadFile(exe)
	if !strings.Contains(string(current), "v1.0.0") {
		t.Error("a binary that does not report the right version was installed anyway")
	}
}

// TestNothingToDoIsNotAnError. The ordinary answer, every night, on every
// machine.
func TestNothingToDoIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	fakeBinary(t, exe, "v1.1.0")

	u, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: "http://example.invalid", SHA256: "abc",
		}},
		Current:    "v1.1.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	release, ok, err := u.Apply(context.Background())
	if ok || err != nil {
		t.Errorf("applied=%v release=%+v err=%v", ok, release, err)
	}
}

// TestAReleaseWithNoHashIsRefused. A release that publishes no SHA256SUMS is
// not installable, however new it is.
func TestAReleaseWithNoHashIsRefused(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	fakeBinary(t, exe, "v1.0.0")

	u, err := selfupdate.New(selfupdate.Config{
		Source:     source{release: selfupdate.Release{Version: "v9.9.9", URL: "http://x.invalid"}},
		Current:    "v1.0.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := u.Apply(context.Background()); ok || !errors.Is(err, selfupdate.ErrNoRelease) {
		t.Errorf("applied=%v err=%v, want ErrNoRelease", ok, err)
	}
}

// TestGitHubReadsTheReleaseAndItsChecksum pins the parsing against the shape
// the API and `make checksums` actually produce.
func TestGitHubReadsTheReleaseAndItsChecksum(t *testing.T) {
	want := selfupdate.AssetName("linux", "amd64")

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{
			  "tag_name": "v1.4.0",
			  "draft": false,
			  "prerelease": false,
			  "assets": [
			    {"name": %q, "browser_download_url": "%s/bin"},
			    {"name": "SHA256SUMS", "browser_download_url": "%s/sums"},
			    {"name": "restic.pin", "browser_download_url": "%s/pin"}
			  ]
			}`, want, srv.URL, srv.URL, srv.URL)

		case r.URL.Path == "/sums":
			// sha256sum's own format, two spaces, other platforms present.
			fmt.Fprintf(w, "aaaa  sion-backup-darwin-arm64\nbbbb  %s\ncccc  sion-backup-windows-amd64.exe\n", want)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Point the client at the test server by overriding the API host through
	// the repository field is not possible, so the source is exercised via
	// its own HTTP client against a rewritten transport.
	gh := selfupdate.GitHub{
		Repository: "jroedel/sion-backup",
		HTTP:       &http.Client{Transport: rewrite{to: srv.URL}},
	}

	release, err := gh.Latest(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	if release.Version != "v1.4.0" {
		t.Errorf("version = %q", release.Version)
	}

	if release.SHA256 != "bbbb" {
		t.Errorf("sha256 = %q, want the line for this platform", release.SHA256)
	}

	if !strings.HasSuffix(release.URL, "/bin") {
		t.Errorf("url = %q", release.URL)
	}
}

// rewrite sends every request to one host, so the GitHub source can be tested
// without reaching GitHub.
type rewrite struct{ to string }

func (t rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	to, err := http.NewRequest(req.Method, t.to+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}

	to.Header = req.Header

	return http.DefaultTransport.RoundTrip(to.WithContext(req.Context()))
}
