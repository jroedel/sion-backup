package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultRepository is where releases come from.
const DefaultRepository = "jroedel/sion-backup"

// GitHub reads the latest release from the GitHub API.
//
// It is the fallback source, and the weaker one: the binary and the
// SHA256SUMS that vouches for it are published by the same workflow to the
// same host, so a hash from there proves the download arrived intact and
// nothing more. See the package comment, and docs/eumaeus-requests.md §5.2
// for the ask that replaces it.
//
// # Rate limits
//
// Unauthenticated, which GitHub allows at 60 requests an hour per address.
// Thirty machines behind one office address, checking a few times a day, is
// nowhere near it — but a machine that checked on every daemon tick would be,
// which is why the caller throttles rather than this.
type GitHub struct {
	// Repository is "owner/name". Empty means DefaultRepository.
	Repository string

	HTTP *http.Client
}

// errNotFound distinguishes the one status worth a sentence of its own.
var errNotFound = errors.New("selfupdate: not found")

// apiTimeout bounds the two small JSON and text requests. Short: this runs
// before a backup, and a slow answer must not delay one.
const apiTimeout = 30 * time.Second

// maxMetadata bounds the release JSON and the checksum file, neither of which
// is remotely near it.
const maxMetadata = 1 << 20

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`

	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Latest returns the newest published release's build for this platform.
func (g GitHub) Latest(ctx context.Context, goos, goarch string) (Release, error) {
	repo := g.Repository
	if repo == "" {
		repo = DefaultRepository
	}

	var rel ghRelease

	url := "https://api.github.com/repos/" + repo + "/releases/latest"

	if err := g.fetchJSON(ctx, url, &rel); err != nil {
		// A repository with no published release answers 404 here, which is
		// an ordinary state and not a fault — it is the state this one was in
		// the day self-update was written. Said plainly, because the raw
		// message is a bare 404 against an API URL and reads like a bug.
		if errors.Is(err, errNotFound) {
			return Release{}, fmt.Errorf("%w: %s has published no releases yet",
				ErrNoRelease, repo)
		}

		return Release{}, err
	}

	// GitHub excludes drafts and prereleases from /latest already; checked
	// anyway, because a prerelease reaching thirty laptops by accident is not
	// a mistake that announces itself.
	if rel.Draft || rel.Prerelease {
		return Release{}, fmt.Errorf("%w: the latest release is a %s",
			ErrNoRelease, map[bool]string{true: "draft", false: "prerelease"}[rel.Draft])
	}

	want := AssetName(goos, goarch)

	var binary, sums string

	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			binary = a.URL
		case "SHA256SUMS":
			sums = a.URL
		}
	}

	if binary == "" {
		return Release{}, fmt.Errorf("%w: %s has no %s", ErrNoRelease, rel.TagName, want)
	}

	if sums == "" {
		// Refused rather than installed unverified. An unverified binary here
		// is one that reads every file on the machine and holds the
		// credentials to the off-site copy.
		return Release{}, fmt.Errorf("%w: %s publishes no SHA256SUMS", ErrNoRelease, rel.TagName)
	}

	sum, err := g.checksum(ctx, sums, want)
	if err != nil {
		return Release{}, err
	}

	return Release{Version: rel.TagName, URL: binary, SHA256: sum}, nil
}

// checksum pulls one line out of a sha256sum-format file.
func (g GitHub) checksum(ctx context.Context, url, asset string) (string, error) {
	body, err := g.fetch(ctx, url, "text/plain")
	if err != nil {
		return "", err
	}

	for line := range strings.Lines(string(body)) {
		// "<hex>  <name>", as sha256sum writes it.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		if strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("%w: SHA256SUMS does not list %s", ErrNoRelease, asset)
}

func (g GitHub) fetchJSON(ctx context.Context, url string, out any) error {
	body, err := g.fetch(ctx, url, "application/vnd.github+json")
	if err != nil {
		return err
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("selfupdate: reading the release list: %w", err)
	}

	return nil
}

func (g GitHub) fetch(ctx context.Context, url, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "sion-backup")

	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: apiTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", errNotFound, url)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: %s: %s", url, resp.Status)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxMetadata))
}

var _ Source = GitHub{}
