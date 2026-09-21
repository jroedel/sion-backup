package restic

import (
	"net/url"
	"regexp"
	"strings"
)

// HostOf is the machine a repository URL names, or "" for one that names none.
//
// For the check that asks whether this computer can look the repository up
// before blaming restic for not reaching it. A repository URL is a backend
// prefix and then something that is nearly a URL —
// "s3:https://s3.us-central-1.wasabisys.com/bucket" — which url.Parse reads as
// the scheme "s3" and an opaque remainder, so the prefix comes off first.
func HostOf(repository string) string {
	s := strings.TrimSpace(repository)

	if i := strings.Index(s, ":"); i >= 0 {
		if rest := s[i+1:]; strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			s = rest
		}
	}

	u, err := url.Parse(s)
	if err != nil {
		return ""
	}

	if h := u.Hostname(); h != "" {
		return h
	}

	// The shorthand, "s3:s3.amazonaws.com/bucket", which has no scheme for
	// url.Parse to find a host after — it reads the backend as the scheme and
	// hands the rest back as opaque. The first segment of that is the host if
	// it looks like a name rather than a directory.
	rest := s
	if u.Opaque != "" {
		rest = u.Opaque
	}

	if strings.HasPrefix(rest, "/") {
		return ""
	}

	if first, _, _ := strings.Cut(rest, "/"); strings.Contains(first, ".") {
		return first
	}

	return ""
}

// lookupFailure matches the name in Go's resolver errors, which is what
// restic's output carries because restic is a Go program too:
//
//	dial tcp: lookup s3.us-central-1.wasabisys.com on 127.0.0.53:53: i/o timeout
//	lookup terraboskamp.org: no such host
var lookupFailure = regexp.MustCompile(`lookup ([A-Za-z0-9._-]+)`)

// Unresolvable reports the host this failure could not look up.
//
// Worth telling apart from every other way of not reaching a bucket. A
// machine whose DNS is broken produces hundreds of lines of retry and backoff,
// none of which say "this computer cannot resolve names" — and the fault is
// not in the backup, the network cable, or the credentials, so somebody
// reading the tail of that output has no reason to look where the problem is.
func (e *Error) Unresolvable() (string, bool) {
	m := lookupFailure.FindStringSubmatch(e.Stderr)
	if m == nil {
		return "", false
	}

	return strings.TrimSuffix(m[1], "."), true
}
