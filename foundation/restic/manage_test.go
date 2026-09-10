package restic

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeAsset is a bzip2-compressed shell script that answers `version` the way
// restic does. It stands in for the 20 MB download in every test here.
//
// A fixture rather than something generated at test time because the standard
// library can read bzip2 and not write it, and the alternative — testing only
// the zip path — would leave the branch every Unix machine actually takes
// uncovered. testdata/fake-restic.bz2 was made with `bzip2 -k`.
const fakeVersion = "9.9.9"

// requireScriptExec skips on Windows, where a shell script is not something
// the smoke test can run.
func requireScriptExec(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake restic is a shell script")
	}
}

func readFixture(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "fake-restic.bz2"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	return raw
}

func sum(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

// serve answers one asset, and reports how many times it was asked for.
func serve(t *testing.T, name string, body []byte) (*httptest.Server, *int) {
	t.Helper()

	var hits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, name) {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		hits++

		_, _ = w.Write(body)
	}))

	t.Cleanup(srv.Close)

	return srv, &hits
}

func fixturePin(t *testing.T, srv *httptest.Server, body []byte) Pin {
	t.Helper()

	return Pin{
		Version: fakeVersion,
		Asset:   "fake-restic.bz2",
		URL:     srv.URL + "/fake-restic.bz2",
		SHA256:  sum(body),
		Binary:  "restic",
	}
}

// TestInstallDownloadsVerifiesAndRuns is the whole path: fetch, hash, extract,
// run, move into place.
func TestInstallDownloadsVerifiesAndRuns(t *testing.T) {
	requireScriptExec(t)

	body := readFixture(t)
	srv, hits := serve(t, "fake-restic.bz2", body)
	dest := filepath.Join(t.TempDir(), "bin", "restic")

	if err := install(t.Context(), dest, fixturePin(t, srv, body), srv.Client()); err != nil {
		t.Fatalf("install: %v", err)
	}

	if *hits != 1 {
		t.Errorf("asked for the asset %d times, want 1", *hits)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("nothing was installed: %v", err)
	}

	// Owner-only. It sits in the directory that holds the machine token, and
	// it is run by exactly one account.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("installed mode %v, want 0700", info.Mode().Perm())
	}

	r := &Runner{bin: dest, managed: true}

	got, err := r.InstalledVersion(t.Context())
	if err != nil {
		t.Fatalf("the installed binary does not answer: %v", err)
	}

	if got != fakeVersion {
		t.Errorf("installed version %q, want %q", got, fakeVersion)
	}
}

// TestInstallRefusesAWrongHash is the check the whole file exists for. A
// binary that reads every file on the machine and holds the credentials to
// the off-site copy is not run because it was nearly right.
func TestInstallRefusesAWrongHash(t *testing.T) {
	body := readFixture(t)
	srv, _ := serve(t, "fake-restic.bz2", body)

	pin := fixturePin(t, srv, body)
	pin.SHA256 = strings.Repeat("0", 64)

	dir := t.TempDir()
	dest := filepath.Join(dir, "bin", "restic")

	err := install(t.Context(), dest, pin, srv.Client())
	if err == nil {
		t.Fatal("installed a binary whose hash did not match the pin")
	}

	if !strings.Contains(err.Error(), "REFUSING") {
		t.Errorf("error %q does not say it refused", err)
	}

	// Nothing left behind, including the staging directory: a half-installed
	// binary where the next step expects a good one is worse than none,
	// because the next step will run it.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a rejected download was left at %s", dest)
	}

	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("reading the install directory: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("a rejected download left %d file(s) behind: %v", len(entries), entries)
	}
}

// TestInstallRefusesABinaryThatReportsSomethingElse covers the check after the
// hash: the right bytes for the wrong platform, or an archive of something
// that is not restic.
func TestInstallRefusesABinaryThatReportsSomethingElse(t *testing.T) {
	requireScriptExec(t)

	body := readFixture(t)
	srv, _ := serve(t, "fake-restic.bz2", body)

	pin := fixturePin(t, srv, body)
	pin.Version = "0.19.1" // the fixture says 9.9.9

	dest := filepath.Join(t.TempDir(), "bin", "restic")

	err := install(t.Context(), dest, pin, srv.Client())
	if err == nil {
		t.Fatal("installed a binary that reports a different version")
	}

	if !strings.Contains(err.Error(), "the pin says 0.19.1") {
		t.Errorf("error %q does not name what was expected", err)
	}
}

// TestInstallReplacesAndKeepsGoing is the version bump: something is already
// there, and it is running the fleet's previous restic.
func TestInstallReplacesAndKeepsGoing(t *testing.T) {
	requireScriptExec(t)

	body := readFixture(t)
	srv, _ := serve(t, "fake-restic.bz2", body)

	dir := t.TempDir()
	dest := filepath.Join(dir, "restic")

	if err := os.WriteFile(dest, []byte("#!/bin/sh\necho restic 0.0.1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := install(t.Context(), dest, fixturePin(t, srv, body), srv.Client()); err != nil {
		t.Fatalf("install over an existing binary: %v", err)
	}

	r := &Runner{bin: dest, managed: true}

	got, err := r.InstalledVersion(t.Context())
	if err != nil {
		t.Fatalf("the replacement does not answer: %v", err)
	}

	if got != fakeVersion {
		t.Errorf("after replacing, version is %q, want %q", got, fakeVersion)
	}
}

// TestExtractZip covers the Windows asset shape, which is the one branch a
// Linux CI would otherwise never take.
func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	// Two entries, and only one of them the executable: upstream ships the
	// binary beside nothing much, but the code picks by suffix and that is
	// worth holding still.
	readme, err := zw.Create("README.md")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := readme.Write([]byte("not the binary")); err != nil {
		t.Fatal(err)
	}

	exe, err := zw.Create("restic_9.9.9_windows_amd64.exe")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := exe.Write([]byte("MZ this is a binary")); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archive := filepath.Join(dir, "restic.zip")

	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "restic.exe")

	if err := extract(archive, dest, Pin{Asset: "restic.zip", Binary: "restic.exe"}); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "MZ this is a binary" {
		t.Errorf("extracted %q", got)
	}
}

// TestEnsurePinnedIsQuietWhenTheVersionIsAlreadyRight is what happens before
// every backup on every machine: one exec, no network. The test proves the
// no-network part by pointing the pin at a server that would fail the test if
// it were asked.
func TestEnsurePinnedIsQuietWhenTheVersionIsAlreadyRight(t *testing.T) {
	requireScriptExec(t)

	dir := t.TempDir()
	dest := filepath.Join(dir, "restic")

	script := "#!/bin/sh\necho \"restic " + PinnedVersion + " compiled with go1.26 on test/test\"\n"

	if err := os.WriteFile(dest, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve("", dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	client := &http.Client{Transport: refuseTransport{t}}

	out, err := r.EnsurePinned(t.Context(), client, nil)
	if err != nil {
		t.Fatalf("EnsurePinned: %v", err)
	}

	if out.Action != ActionAlready {
		t.Errorf("action %q, want %q", out.Action, ActionAlready)
	}

	if out.To != PinnedVersion {
		t.Errorf("version %q, want %q", out.To, PinnedVersion)
	}
}

// refuseTransport fails the test if anything reaches the network.
type refuseTransport struct{ t *testing.T }

func (r refuseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("the network was used, for %s", req.URL)

	return nil, http.ErrUseLastResponse
}

// TestEnsurePinnedLeavesAConfiguredBinaryAlone is the operator's escape hatch:
// naming a path in the config file is taking its version on, and this program
// must not then replace it.
func TestEnsurePinnedLeavesAConfiguredBinaryAlone(t *testing.T) {
	requireScriptExec(t)

	dir := t.TempDir()
	named := filepath.Join(dir, "my-restic")

	if err := os.WriteFile(named, []byte("#!/bin/sh\necho restic 0.0.1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(named, filepath.Join(dir, "bin"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if r.Managed() {
		t.Error("a binary named in the config file is reported as managed")
	}

	out, err := r.EnsurePinned(t.Context(), &http.Client{Transport: refuseTransport{t}}, nil)
	if err != nil {
		t.Fatalf("EnsurePinned: %v", err)
	}

	if out.Action != ActionOverride {
		t.Errorf("action %q, want %q", out.Action, ActionOverride)
	}
}

// TestResolveDoesNotRequireTheBinaryToExist is what lets a fresh machine wire
// itself up before it has downloaded anything.
func TestResolveDoesNotRequireTheBinaryToExist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")

	r, err := Resolve("", dir)
	if err != nil {
		t.Fatalf("Resolve on a machine with no restic: %v", err)
	}

	if !r.Managed() {
		t.Error("the fleet's own copy is not reported as managed")
	}

	if filepath.Dir(r.Bin()) != dir {
		t.Errorf("Bin() is %s, want it inside %s", r.Bin(), dir)
	}
}

// TestResolveRejectsAConfiguredBinaryThatIsNotThere fails early rather than at
// two in the morning: a typo in the config file is a thing to hear about now.
func TestResolveRejectsAConfiguredBinaryThatIsNotThere(t *testing.T) {
	if _, err := Resolve(filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
		t.Fatal("Resolve accepted a configured binary that does not exist")
	}
}
