package s3probe

import (
	"fmt"
	"net/url"
	"strings"
)

// ProbeKey is the object key every probe uses.
//
// One fixed name rather than a random one, and it is named so that anybody who
// ever sees it in a bucket listing knows what it was — though they will not,
// because the upload is aborted and never becomes an object. It exists for the
// case where an abort fails and somebody is later looking at a list of
// incomplete multipart uploads wondering what to do about them. The answer is:
// delete them, nothing here needs them.
const ProbeKey = "sion-backup-upload-speed-probe"

// TargetFor works out where to probe from the repository this machine writes
// to.
//
// The URL is restic's, which is an S3 endpoint with the bucket as the first
// path segment and the repository possibly under a prefix inside it:
//
//	s3:https://s3.us-central-1.wasabisys.com/dell3-backup
//	s3:https://s3.us-central-1.wasabisys.com/shared-bucket/dell3
//
// The probe goes beside the repository rather than inside it — under the same
// prefix, at [ProbeKey] — so that a bucket holding several machines' backups
// does not get a probe key inside one machine's repository directory.
func TargetFor(repositoryURL string) (Target, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(repositoryURL), "s3:")
	if !ok {
		return Target{}, fmt.Errorf("s3probe: %q is not an s3 repository URL", repositoryURL)
	}

	// restic accepts a bare host, meaning https. Everything this fleet writes
	// carries the scheme, but a config file may not.
	if !strings.Contains(rest, "://") {
		rest = "https://" + rest
	}

	u, err := url.Parse(rest)
	if err != nil {
		return Target{}, fmt.Errorf("s3probe: reading the repository URL: %w", err)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if parts[0] == "" {
		return Target{}, fmt.Errorf("s3probe: %q names no bucket", repositoryURL)
	}

	key := ProbeKey
	if prefix := strings.Join(parts[1:], "/"); prefix != "" {
		key = prefix + "/" + ProbeKey
	}

	return Target{
		Endpoint: u.Scheme + "://" + u.Host,
		Region:   RegionOf(u.Hostname()),
		Bucket:   parts[0],
		Key:      key,
	}, nil
}

// DefaultRegion is what an endpoint that does not name one is signed under.
//
// us-east-1 rather than an error. It is what both Wasabi's and AWS's
// region-less endpoints actually are, and it is what every S3 client in the
// world falls back to — including the minio-go inside restic, which is the
// thing this probe has to agree with.
const DefaultRegion = "us-east-1"

// RegionOf reads the region out of an S3 endpoint host.
//
// The convention is `s3.<region>.<domain>` — s3.us-central-1.wasabisys.com,
// s3.eu-west-2.amazonaws.com — with the region-less form meaning
// [DefaultRegion].
//
// A guess, and a cheap one to get wrong: a probe signed under the wrong region
// is refused by the endpoint with a message that names the right one, the page
// says the speed could not be measured, and nothing else in the program is
// affected. That is why this is a string match rather than a table of
// providers that would go stale.
func RegionOf(host string) string {
	labels := strings.Split(strings.ToLower(host), ".")

	// Fewer than three labels is not a hosted S3 endpoint at all — "localhost",
	// or a test server — and has no region to find.
	if len(labels) < 3 {
		return DefaultRegion
	}

	// The second label is the region only when the first is the service. A
	// virtual-host style name (bucket.s3.region.example) puts the bucket
	// first, so the service label is looked for rather than assumed.
	for i, label := range labels {
		if label != "s3" || i+1 >= len(labels) {
			continue
		}

		if candidate := labels[i+1]; looksLikeRegion(candidate) {
			return candidate
		}

		return DefaultRegion
	}

	return DefaultRegion
}

// looksLikeRegion reports whether a label has the shape of an AWS-style region
// name: letters, then a dash, then more, ending in a digit. "us-central-1"
// yes; "wasabisys" no.
func looksLikeRegion(s string) bool {
	if !strings.Contains(s, "-") {
		return false
	}

	last := s[len(s)-1]

	return last >= '0' && last <= '9'
}
