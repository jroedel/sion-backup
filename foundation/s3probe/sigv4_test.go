package s3probe

import (
	"encoding/hex"
	"net/url"
	"testing"
)

// TestTheSigningChainMatchesTheWorkedExample checks all three steps against
// the numbers AWS publishes.
//
// # Why the example is an IAM one in a package that only signs for S3
//
// Because it is the one with every intermediate value written down. AWS's
// SigV4 documentation walks a single request — ListUsers against
// iam.amazonaws.com, on 30 August 2015 — and prints the canonical request, its
// hash, the derived signing key and the final signature. That makes it the
// only test available here that is not this package checking its own
// arithmetic: every constant below came from the specification, and none of
// them from running this code.
//
// The service is therefore "iam" and not "s3", which is exactly why
// signingKey takes it as a parameter.
func TestTheSigningChainMatchesTheWorkedExample(t *testing.T) {
	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		stamp  = "20150830T123600Z"
		day    = "20150830"
		scope  = "20150830/us-east-1/iam/aws4_request"

		wantCanonicalHash = "f536975d06c0309214f805bb90ccff089219ecd68b2577efef23edd43b7e1a59"
		wantKey           = "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
		wantSignature     = "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	)

	headers := "content-type:application/x-www-form-urlencoded; charset=utf-8\n" +
		"host:iam.amazonaws.com\n" +
		"x-amz-date:" + stamp + "\n"

	canonical := canonicalRequest("GET", "/", "Action=ListUsers&Version=2010-05-08",
		headers, "content-type;host;x-amz-date", emptyPayload)

	if got := hashHex([]byte(canonical)); got != wantCanonicalHash {
		t.Errorf("the canonical request hashes to %s, and the specification says %s\n\n%q",
			got, wantCanonicalHash, canonical)
	}

	key := signingKey([]byte(secret), day, "us-east-1", "iam")
	if got := hex.EncodeToString(key); got != wantKey {
		t.Errorf("the signing key is %s, and the specification says %s", got, wantKey)
	}

	sts := stringToSign(stamp, scope, canonical)

	if got := hex.EncodeToString(hmacSHA256(key, []byte(sts))); got != wantSignature {
		t.Errorf("the signature is %s, and the specification says %s\n\nstring to sign:\n%q",
			got, wantSignature, sts)
	}
}

// TestTheCanonicalHeadersLeaveABlankLine guards the one part of the canonical
// request that is invisible: the header block ends with a newline of its own,
// so joining puts an empty line before the signed-header list. Dropping it
// produces a signature that fails with a message about the whole request,
// which is how a whole afternoon gets spent.
func TestTheCanonicalHeadersLeaveABlankLine(t *testing.T) {
	got := canonicalRequest("PUT", "/b/k", "partNumber=1", "host:example\n", "host", unsignedPayload)

	const want = "PUT\n/b/k\npartNumber=1\nhost:example\n\nhost\nUNSIGNED-PAYLOAD"

	if got != want {
		t.Errorf("canonical request\n got %q\nwant %q", got, want)
	}
}

func TestQueriesAreEncodedTheWayTheSignatureExpects(t *testing.T) {
	for _, c := range []struct {
		name string
		in   url.Values
		want string
	}{
		{
			// The parameter that begins a multipart upload has no value, and
			// it still needs its "=". A bare "uploads" is a different
			// canonical request from the one the endpoint builds.
			name: "a valueless parameter keeps its equals sign",
			in:   url.Values{"uploads": []string{""}},
			want: "uploads=",
		},
		{
			name: "parameters are sorted by name, not left in map order",
			in:   url.Values{"uploadId": []string{"abc"}, "partNumber": []string{"2"}},
			want: "partNumber=2&uploadId=abc",
		},
		{
			// url.Values.Encode renders this as "+", which SigV4 refuses.
			name: "a space is %20 and never a plus",
			in:   url.Values{"uploadId": []string{"a b"}},
			want: "uploadId=a%20b",
		},
		{
			// Upload IDs are opaque and routinely contain these.
			name: "the unreserved set is left alone and everything else escaped",
			in:   url.Values{"uploadId": []string{"aZ0-_.~/+="}},
			want: "uploadId=aZ0-_.~%2F%2B%3D",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := encodeQuery(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestTheProbeTargetComesOutOfTheRepositoryURL(t *testing.T) {
	for _, c := range []struct {
		name string
		url  string
		want Target
	}{
		{
			name: "a bucket at the root of the endpoint",
			url:  "s3:https://s3.us-central-1.wasabisys.com/dell3-backup",
			want: Target{
				Endpoint: "https://s3.us-central-1.wasabisys.com",
				Region:   "us-central-1",
				Bucket:   "dell3-backup",
				Key:      ProbeKey,
			},
		},
		{
			// The probe goes beside the repository, under the same prefix, so
			// a bucket holding several machines does not get a probe key
			// inside one machine's repository directory.
			name: "a repository under a prefix",
			url:  "s3:https://s3.eu-west-2.wasabisys.com/shared/dell3/restic",
			want: Target{
				Endpoint: "https://s3.eu-west-2.wasabisys.com",
				Region:   "eu-west-2",
				Bucket:   "shared",
				Key:      "dell3/restic/" + ProbeKey,
			},
		},
		{
			name: "an endpoint that names no region",
			url:  "s3:https://s3.wasabisys.com/dell3-backup",
			want: Target{
				Endpoint: "https://s3.wasabisys.com",
				Region:   DefaultRegion,
				Bucket:   "dell3-backup",
				Key:      ProbeKey,
			},
		},
		{
			// restic accepts a bare host and means https by it.
			name: "no scheme means https",
			url:  "s3:s3.us-central-1.wasabisys.com/dell3-backup",
			want: Target{
				Endpoint: "https://s3.us-central-1.wasabisys.com",
				Region:   "us-central-1",
				Bucket:   "dell3-backup",
				Key:      ProbeKey,
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := TargetFor(c.url)
			if err != nil {
				t.Fatalf("TargetFor(%q): %v", c.url, err)
			}

			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestARepositoryThatIsNotS3IsRefused(t *testing.T) {
	for _, url := range []string{
		"/srv/backup",
		"sftp:user@host:/backup",
		"s3:https://s3.wasabisys.com",
		"",
	} {
		if _, err := TargetFor(url); err == nil {
			t.Errorf("TargetFor(%q) returned no error, and there is nothing to probe there", url)
		}
	}
}
