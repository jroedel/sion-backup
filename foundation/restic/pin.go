package restic

import (
	"fmt"
	"runtime"
)

// PinnedVersion is the restic build every machine in this fleet runs.
//
// Exactly this version, not a floor. The fleet manages its own copy (see
// EnsurePinned), so "which restic wrote this snapshot" is a fact recorded on
// every run rather than a guess about whatever a package manager happened to
// install — and the answer is the same on all of them, which is what makes a
// restore card printable and a bug report reproducible.
//
// It is bumped by editing this file and deploy/restic.pin together;
// TestPinMatchesDeployFile fails if only one of them moves. A machine picks
// the new version up the next time it self-updates, because the pin travels
// inside the binary: one release channel, not two.
const PinnedVersion = "0.19.1"

// pinnedSHA256 is what each platform's release asset must hash to.
//
// Copied verbatim from the upstream SHA256SUMS, which restic signs — see
// deploy/restic.pin for the signature check that belongs with a bump. These
// hashes are the whole of the trust in a binary that reads every file on
// somebody's computer and holds the credentials to their off-site copy, so a
// platform missing from this map cannot be installed at all rather than
// installed unverified.
var pinnedSHA256 = map[string]string{
	"linux/amd64":   "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c",
	"linux/arm64":   "a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465",
	"darwin/amd64":  "c38d579622cf602f665234c5a8c315030b6cf70656028fe6dc29a786b60e5f35",
	"darwin/arm64":  "7be0a144ccc377880f294204aa271d76e4b79554b42a751151d425ce6ebac143",
	"windows/amd64": "da948ad707ed690426473aaba2046cd61f8f90f6f0e7dab6be0d5796531de67d",
}

// ErrNoPin reports a platform the pin does not cover. It is returned rather
// than falling back to an unverified download: see pinnedSHA256.
var ErrNoPin = fmt.Errorf("restic: no pinned hash for this platform")

// Pin is the pinned build for one platform.
type Pin struct {
	// Version is the restic release, "0.19.1", without a leading v.
	Version string

	// Asset is the file name upstream publishes, which is also the name the
	// hash in SHA256SUMS is against.
	Asset string

	// URL is where that asset lives.
	URL string

	// SHA256 is what it must hash to, lower-case hex.
	SHA256 string

	// Binary is what the extracted executable is called once installed.
	Binary string
}

// PinFor returns the pinned build for a platform, or ErrNoPin.
func PinFor(goos, goarch string) (Pin, error) {
	sum, ok := pinnedSHA256[goos+"/"+goarch]
	if !ok {
		return Pin{}, fmt.Errorf("%w: %s/%s; add one from a signed upstream "+
			"SHA256SUMS to foundation/restic/pin.go and deploy/restic.pin",
			ErrNoPin, goos, goarch)
	}

	ext, binary := "bz2", "restic"
	if goos == "windows" {
		ext, binary = "zip", "restic.exe"
	}

	asset := fmt.Sprintf("restic_%s_%s_%s.%s", PinnedVersion, goos, goarch, ext)

	return Pin{
		Version: PinnedVersion,
		Asset:   asset,
		URL: fmt.Sprintf("https://github.com/restic/restic/releases/download/v%s/%s",
			PinnedVersion, asset),
		SHA256: sum,
		Binary: binary,
	}, nil
}

// pinHere is the pinned build for the machine this is running on.
func pinHere() (Pin, error) { return PinFor(runtime.GOOS, runtime.GOARCH) }
