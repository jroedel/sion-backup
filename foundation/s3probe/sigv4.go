package s3probe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The two values that can appear in x-amz-content-sha256.
const (
	// emptyPayload is sha256(""), which is what a request with no body signs.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// unsignedPayload tells the endpoint not to expect a hash of the body.
	// Permitted over HTTPS, and what the timed request uses: see prober.part.
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

// algorithm names the signing scheme in both the string to sign and the header.
const algorithm = "AWS4-HMAC-SHA256"

// service is what every request this package makes is signed for.
const service = "s3"

// sign adds the SigV4 Authorization header to req.
//
// # The algorithm, since this is the only place it appears
//
// Four steps, and the awkwardness in all four is that both ends have to agree
// on a byte-exact rendering of the request before either can hash it.
//
//  1. The canonical request: method, path, query, the headers being signed,
//     and the payload hash, each normalised — the path and query percent-
//     encoded to one fixed convention, the headers lower-cased and sorted.
//  2. The string to sign: the algorithm, the timestamp, the credential scope
//     (date, region, service) and a hash of the canonical request.
//  3. The signing key: HMAC applied four times, starting from the secret and
//     folding in the date, the region, the service and a fixed terminator. It
//     is derived rather than used directly so that a leaked signature is good
//     for one region on one day.
//  4. HMAC the string to sign with that key, and say so in a header naming
//     exactly which headers were covered.
//
// Only three headers are signed — host, x-amz-date and x-amz-content-sha256.
// Signing a subset is legal and deliberate: Content-Length is set by net/http
// after this runs, and a signature over a header the transport may still
// change is a signature that fails for reasons nobody can see.
func sign(req *http.Request, c Credentials, region, payload string, now time.Time) error {
	if len(c.AccessKeyID) == 0 || len(c.SecretAccessKey) == 0 {
		return fmt.Errorf("s3probe: the S3 credentials are incomplete")
	}

	now = now.UTC()

	stamp := now.Format("20060102T150405Z")
	day := now.Format("20060102")

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)

	// Host is not in req.Header — net/http carries it on the request — so it
	// is read from the URL, which is also where the far end will read it from.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payload + "\n" +
		"x-amz-date:" + stamp + "\n"

	canonical := canonicalRequest(req.Method, encodePath(req.URL.Path), req.URL.RawQuery,
		canonicalHeaders, signedHeaders, payload)

	scope := day + "/" + region + "/" + service + "/aws4_request"

	key := signingKey(c.SecretAccessKey, day, region, service)
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign(stamp, scope, canonical))))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.AccessKeyID, scope, signedHeaders, signature))

	return nil
}

// canonicalRequest is step 1: the request, rendered so that both ends produce
// the same bytes.
//
// canonicalHeaders carries its own trailing newline, which is why joining
// leaves a blank line before the signed-header list. That blank line is part
// of the format rather than an accident of the join, and it is the sort of
// detail that makes a signature fail with a message about the whole request.
func canonicalRequest(method, path, query, canonicalHeaders, signedHeaders, payload string) string {
	if path == "" {
		path = "/"
	}

	return strings.Join([]string{
		method, path, query, canonicalHeaders, signedHeaders, payload,
	}, "\n")
}

// stringToSign is step 2: what the signature is actually computed over.
func stringToSign(stamp, scope, canonical string) string {
	return strings.Join([]string{algorithm, stamp, scope, hashHex([]byte(canonical))}, "\n")
}

// signingKey is step 3: HMAC applied four times, so that a leaked signature is
// good for one region, one service and one day.
//
// The service is a parameter although this program only ever signs for S3.
// That is not generality for its own sake: AWS publishes worked examples of
// this chain for `iam`, and a parameter is what lets the test assert against
// the published numbers rather than against this function's own output.
func signingKey(secret []byte, day, region, service string) []byte {
	k := hmacSHA256(append([]byte("AWS4"), secret...), []byte(day))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))

	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)

	return m.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// encodeQuery renders a query string the way the signature expects it.
//
// url.Values.Encode is nearly this and not quite: it renders a space as "+",
// and SigV4 requires "%20". The probe's own parameters contain no spaces, so
// this is about the rule rather than about a value we send — but a canonical
// encoder that is right for the values it happens to be given today is how the
// next parameter breaks the signature.
func encodeQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var parts []string

	for _, k := range keys {
		for _, value := range v[k] {
			parts = append(parts, encodeSegment(k)+"="+encodeSegment(value))
		}
	}

	return strings.Join(parts, "&")
}

// encodePath percent-encodes a URI path, leaving the separators alone.
func encodePath(path string) string {
	if path == "" {
		return "/"
	}

	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = encodeSegment(s)
	}

	return strings.Join(segments, "/")
}

// encodeSegment percent-encodes everything outside RFC 3986's unreserved set.
//
// Written out rather than taken from net/url because the standard library's
// escapers each encode for a particular context and none of them for this one:
// url.QueryEscape turns a space into "+", and url.PathEscape leaves several
// characters unescaped that SigV4 requires escaped. A canonical request that
// disagrees with the far end by one byte fails as "signature does not match",
// which says nothing about which byte.
func encodeSegment(s string) string {
	const hexDigits = "0123456789ABCDEF"

	var b strings.Builder

	for i := range len(s) {
		ch := s[i]

		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)

		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[ch>>4])
			b.WriteByte(hexDigits[ch&0x0f])
		}
	}

	return b.String()
}
