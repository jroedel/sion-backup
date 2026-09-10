package restic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPinMatchesDeployFile is the guard on the one duplicated fact in this
// repository.
//
// The pin has to be readable two ways: by this program, which installs restic
// on machines that have none, and by deploy/restic.pin, which is
// shell-sourceable and is what the installers, the Makefile and CI read
// before there is a Go toolchain in the picture. Neither can be derived from
// the other at build time without making one of them depend on the other's
// tooling.
//
// So there are two copies and this test makes drifting between them a build
// failure. A bump edits both, or it does not land.
func TestPinMatchesDeployFile(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "restic.pin")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	got := map[string]string{}

	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("%s: cannot read the line %q", path, line)

			continue
		}

		got[name] = value
	}

	if got["RESTIC_VERSION"] != PinnedVersion {
		t.Errorf("%s pins restic %s and pin.go pins %s; bump both",
			path, got["RESTIC_VERSION"], PinnedVersion)
	}

	for platform, want := range pinnedSHA256 {
		goos, goarch, _ := strings.Cut(platform, "/")
		name := "RESTIC_SHA256_" + goos + "_" + goarch

		if got[name] != want {
			t.Errorf("%s has %s=%s, pin.go has %s; bump both", path, name, got[name], want)
		}

		delete(got, name)
	}

	delete(got, "RESTIC_VERSION")

	// The other direction: a platform added to the shell file and forgotten
	// here is a machine the installers can provision and this program cannot.
	for name := range got {
		t.Errorf("%s has %s and pin.go has no matching entry", path, name)
	}
}

// TestPinForNamesTheUpstreamAsset holds the URL shape still. It is built from
// the version rather than stored, so a bump that gets it wrong would 404 on
// every machine at once.
func TestPinForNamesTheUpstreamAsset(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		asset        string
		binary       string
	}{
		{"linux", "amd64", "restic_" + PinnedVersion + "_linux_amd64.bz2", "restic"},
		{"darwin", "arm64", "restic_" + PinnedVersion + "_darwin_arm64.bz2", "restic"},
		{"windows", "amd64", "restic_" + PinnedVersion + "_windows_amd64.zip", "restic.exe"},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			pin, err := PinFor(tc.goos, tc.goarch)
			if err != nil {
				t.Fatalf("PinFor: %v", err)
			}

			if pin.Asset != tc.asset {
				t.Errorf("asset %q, want %q", pin.Asset, tc.asset)
			}

			if pin.Binary != tc.binary {
				t.Errorf("binary %q, want %q", pin.Binary, tc.binary)
			}

			want := "https://github.com/restic/restic/releases/download/v" +
				PinnedVersion + "/" + tc.asset

			if pin.URL != want {
				t.Errorf("url %q, want %q", pin.URL, want)
			}

			if len(pin.SHA256) != 64 {
				t.Errorf("sha256 %q is not 64 hex characters", pin.SHA256)
			}
		})
	}
}

// TestPinForRefusesAnUnpinnedPlatform is the fail-closed case: a platform
// nobody has copied a signed hash for cannot be installed unverified.
func TestPinForRefusesAnUnpinnedPlatform(t *testing.T) {
	_, err := PinFor("plan9", "mips")
	if !errors.Is(err, ErrNoPin) {
		t.Fatalf("PinFor on an unpinned platform returned %v, want ErrNoPin", err)
	}
}

func TestVersionNumber(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"restic 0.19.1 compiled with go1.26.4 on linux/amd64", "0.19.1"},
		{"restic 0.14.0 compiled with go1.19 on linux/amd64", "0.14.0"},
		{"restic 0.19.1", "0.19.1"},

		// A build somebody made themselves is not the release, and must not
		// be mistaken for it: this fleet runs the pinned version exactly.
		{"restic 0.19.1-dev compiled with go1.26 on linux/amd64", ""},
		// restic does not print a v, but a number that carries one is still
		// that number rather than an unknown build.
		{"restic v0.19.1 compiled with go1.26", "0.19.1"},

		{"", ""},
		{"restic", ""},
		{"borg 1.2.3", ""},
		{"go version go1.26.4 linux/amd64", ""},
	} {
		if got := VersionNumber(tc.in); got != tc.want {
			t.Errorf("VersionNumber(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
