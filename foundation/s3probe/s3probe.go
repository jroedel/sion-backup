// Package s3probe measures how fast this machine can upload to its own bucket.
//
// It answers one question, asked once, by somebody who is about to say yes to
// a first backup: how long is this going to take? Forty gigabytes is two hours
// on an office fibre and four days on a rural DSL line, and the difference
// between those two is the difference between "fine" and "unplug it, I have a
// meeting". Guessing is not good enough, and neither is the download speed a
// browser speed test reports — these connections are asymmetric, and it is the
// upload that this program spends.
//
// # Why it writes nothing
//
// A speed test has to send real bytes to the real endpoint over the real path,
// or it is measuring something else. But nothing in this fleet may delete
// backup data — that is the property docs/model.md §5.4 is built around, and
// the machine key cannot delete objects — so an ordinary PUT would leave a
// junk object in somebody's repository bucket that nothing in the system could
// ever remove.
//
// So the probe is a multipart upload that is never completed:
//
//	POST   /bucket/key?uploads            begin, and get an upload ID
//	PUT    /bucket/key?partNumber=N&...   the bytes, timed
//	DELETE /bucket/key?uploadId=...       abort
//
// An aborted multipart upload has no object at the end of it and no parts left
// behind. It uses exactly two permissions — PutObject and
// AbortMultipartUpload — and restic itself requires both, because that is how
// minio-go uploads a pack file and how it cleans up after a failed one. The
// probe therefore needs nothing the machine key does not already have to do
// its actual job.
//
// # Why the signing is written out here
//
// There is no AWS SDK in this program and adding one to send three requests
// would be the larger decision. SigV4 is a hash chain over a canonical string;
// it is written out in [sign], which is the only cryptographic code in this
// repository and is commented accordingly.
package s3probe

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Target is the bucket to probe and where it lives.
type Target struct {
	// Endpoint is the scheme and host, e.g. "https://s3.us-central-1.wasabisys.com".
	Endpoint string

	// Region is what SigV4 signs under.
	Region string

	// Bucket is the bucket name.
	Bucket string

	// Key is the object key the probe uses. It never becomes an object; see
	// the package comment.
	Key string
}

// Credentials are the S3 keys, held for the length of one probe.
//
// []byte for the same reason every other credential in this program is: a
// string cannot be overwritten. This package does not wipe them — the caller
// owns them and already defers that.
type Credentials struct {
	AccessKeyID     []byte
	SecretAccessKey []byte
}

// Result is one measurement.
type Result struct {
	// BytesPerSecond is the rate the timed part achieved.
	BytesPerSecond float64

	// Sent is how many bytes that part carried, and Took how long the request
	// took end to end — connection reuse included, TLS handshake excluded,
	// because the warm-up part pays for that.
	Sent int64
	Took time.Duration

	// MeasuredAt is when it finished, so a page can say how old the figure is.
	MeasuredAt time.Time
}

// Seconds estimates how long it would take to upload n bytes at this rate.
func (r Result) Seconds(n int64) time.Duration {
	if r.BytesPerSecond <= 0 || n <= 0 {
		return 0
	}

	return time.Duration(float64(n) / r.BytesPerSecond * float64(time.Second))
}

// Options tune the probe. The zero value is the one to use.
type Options struct {
	// WarmUp is the size of the first, untimed part. It pays for the TLS
	// handshake and gives a rough rate to size the real one from.
	WarmUp int64

	// Target is roughly how long the timed part should take. The part is
	// sized from the warm-up's rate to hit it, then clamped.
	Target time.Duration

	// MinPart and MaxPart bound that size.
	MinPart int64
	MaxPart int64

	// Client is the HTTP client, for tests. nil uses a shared default.
	Client *http.Client

	// Now is the clock SigV4 signs with, for tests. nil uses time.Now.
	Now func() time.Time
}

// Defaults for [Options], and the reasoning for each.
const (
	// defaultWarmUp is big enough to get past TCP slow start on a slow link
	// and small enough to cost nothing on a fast one.
	defaultWarmUp = 2 << 20 // 2 MiB

	// defaultTarget is how long the timed part aims to take. Eight seconds is
	// long enough that the rate has settled and short enough that somebody
	// pressing "test again" does not wonder whether it worked.
	defaultTarget = 8 * time.Second

	// defaultMinPart keeps the measurement honest on a fast connection, where
	// eight seconds' worth would otherwise be sized from a warm-up that was
	// over before it started.
	defaultMinPart = 8 << 20 // 8 MiB

	// defaultMaxPart is the ceiling on what this costs somebody. On a slow
	// line the part is small and the measurement is short; this bounds the
	// other end, where a gigabit office connection would otherwise send a
	// gigabyte to find out it is fast.
	defaultMaxPart = 64 << 20 // 64 MiB
)

func (o Options) withDefaults() Options {
	if o.WarmUp <= 0 {
		o.WarmUp = defaultWarmUp
	}

	if o.Target <= 0 {
		o.Target = defaultTarget
	}

	if o.MinPart <= 0 {
		o.MinPart = defaultMinPart
	}

	if o.MaxPart <= 0 {
		o.MaxPart = defaultMaxPart
	}

	if o.Client == nil {
		o.Client = shared
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	return o
}

// shared is the client every probe uses unless a test says otherwise. One
// client, so the warm-up's connection is the one the timed part reuses — which
// is the whole reason there is a warm-up.
var shared = &http.Client{Timeout: 5 * time.Minute}

// Measure runs one probe.
//
// The abort at the end runs even when the context has been cancelled, on a
// short context of its own: the one thing worse than a speed test that failed
// is a speed test that failed and left half an object behind.
func Measure(ctx context.Context, t Target, c Credentials, opts Options) (Result, error) {
	opts = opts.withDefaults()

	if err := t.validate(); err != nil {
		return Result{}, err
	}

	p := prober{target: t, creds: c, opts: opts}

	uploadID, err := p.begin(ctx)
	if err != nil {
		return Result{}, err
	}

	defer p.abort(ctx, uploadID)

	// Part one is thrown away. It pays for the TLS handshake, gets TCP past
	// slow start, and produces the rough rate the real part is sized from.
	warm, err := p.part(ctx, uploadID, 1, opts.WarmUp)
	if err != nil {
		return Result{}, err
	}

	size := clamp(int64(warm.BytesPerSecond*opts.Target.Seconds()), opts.MinPart, opts.MaxPart)

	timed, err := p.part(ctx, uploadID, 2, size)
	if err != nil {
		// The warm-up did reach the bucket, so the connection works and this
		// is a real if rougher number. Returning it beats telling somebody
		// nothing could be measured when something was.
		return warm, nil
	}

	return timed, nil
}

// prober is one probe's state.
type prober struct {
	target Target
	creds  Credentials
	opts   Options
}

// begin starts the multipart upload and returns its ID.
func (p prober) begin(ctx context.Context) (string, error) {
	req, err := p.request(ctx, http.MethodPost, url.Values{"uploads": []string{""}}, nil, 0)
	if err != nil {
		return "", err
	}

	body, err := p.do(req, emptyPayload)
	if err != nil {
		return "", fmt.Errorf("s3probe: starting the upload: %w", err)
	}

	var out struct {
		UploadID string `xml:"UploadId"`
	}

	if err := xml.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("s3probe: reading the upload ID: %w", err)
	}

	if out.UploadID == "" {
		return "", fmt.Errorf("s3probe: the endpoint began an upload but named no ID")
	}

	return out.UploadID, nil
}

// part uploads n bytes as one part and times the whole request.
func (p prober) part(ctx context.Context, uploadID string, number int, n int64) (Result, error) {
	q := url.Values{
		"partNumber": []string{strconv.Itoa(number)},
		"uploadId":   []string{uploadID},
	}

	req, err := p.request(ctx, http.MethodPut, q, filler(n), n)
	if err != nil {
		return Result{}, err
	}

	started := time.Now()

	// UNSIGNED-PAYLOAD rather than a hash of the body. Hashing 64 MiB to
	// measure how fast it uploads would put this machine's CPU in the
	// measurement, and the request is inside TLS either way.
	if _, err := p.do(req, unsignedPayload); err != nil {
		return Result{}, fmt.Errorf("s3probe: uploading the test data: %w", err)
	}

	took := time.Since(started)
	if took <= 0 {
		took = time.Millisecond
	}

	return Result{
		BytesPerSecond: float64(n) / took.Seconds(),
		Sent:           n,
		Took:           took,
		MeasuredAt:     time.Now(),
	}, nil
}

// abort throws the upload away.
//
// Best effort, and on its own context: it runs from a defer, and the path that
// gets here most often is the one where the context that was passed in has
// just been cancelled. Nothing is returned because there is nothing a caller
// could usefully do — what a failure here leaves is an incomplete upload with
// no object, which the bucket's own lifecycle rules clear.
func (p prober) abort(ctx context.Context, uploadID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	req, err := p.request(ctx, http.MethodDelete, url.Values{"uploadId": []string{uploadID}}, nil, 0)
	if err != nil {
		return
	}

	_, _ = p.do(req, emptyPayload)
}

// request builds one signed-able request against the probe key.
func (p prober) request(ctx context.Context, method string, query url.Values,
	body io.Reader, length int64) (*http.Request, error) {

	raw := strings.TrimSuffix(p.target.Endpoint, "/") +
		"/" + p.target.Bucket + "/" + p.target.Key + "?" + encodeQuery(query)

	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, fmt.Errorf("s3probe: building the request: %w", err)
	}

	req.ContentLength = length

	return req, nil
}

// do signs, sends, and reads the response to the end.
//
// To the end deliberately, and it is the reason the timing is where it is: a
// PUT is not finished when the last byte leaves this machine, it is finished
// when the far end says so, and a measurement that stopped at the write would
// be measuring the size of somebody's socket buffer.
func (p prober) do(req *http.Request, payload string) ([]byte, error) {
	if err := sign(req, p.creds, p.target.Region, payload, p.opts.Now()); err != nil {
		return nil, err
	}

	resp, err := p.opts.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the endpoint answered %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}

	return body, nil
}

// validate reports a target that could not be signed for.
func (t Target) validate() error {
	switch {
	case t.Endpoint == "":
		return fmt.Errorf("s3probe: no endpoint")
	case t.Bucket == "":
		return fmt.Errorf("s3probe: no bucket")
	case t.Key == "":
		return fmt.Errorf("s3probe: no key")
	case t.Region == "":
		return fmt.Errorf("s3probe: no region")
	}

	return nil
}

func clamp(v, lo, hi int64) int64 {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}

// block is the data the probe sends, generated once.
//
// Random rather than zeroes. Nothing on this path should be able to compress
// it — not the TLS layer, not a middlebox, not a helpful proxy — because a
// connection that looks twice as fast for a buffer of zeroes than for
// somebody's photographs is worse than no measurement.
var block = func() []byte {
	b := make([]byte, 1<<20)

	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform this runs on, and if it
		// did, a measurement against a compressible buffer is still a
		// measurement. It is not worth a returned error through three layers.
		for i := range b {
			b[i] = byte(i * 31)
		}
	}

	return b
}()

// filler returns a reader of exactly n bytes.
func filler(n int64) io.Reader {
	return io.LimitReader(repeat{}, n)
}

// repeat is an endless stream of the block above.
type repeat struct{}

func (repeat) Read(p []byte) (int, error) {
	n := copy(p, block)

	for n < len(p) {
		n += copy(p[n:], block)
	}

	return n, nil
}
