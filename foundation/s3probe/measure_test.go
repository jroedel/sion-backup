package s3probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is enough of a multipart endpoint to drive a probe, and it records
// what the probe did so a test can assert on it.
type fakeS3 struct {
	mu sync.Mutex

	// calls is every request, as "METHOD path?query".
	calls []string

	// parts maps part number to the number of bytes actually received. The
	// bytes are counted rather than kept: a probe sends tens of megabytes and
	// a test that held them would be measuring the runner's memory.
	parts map[string]int64

	aborted bool

	// failPart, when set, is the part number to refuse, for the test that a
	// failed measurement still falls back and still cleans up.
	failPart string

	// delay is how long each part upload takes, so a rate can be asserted.
	delay time.Duration
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()

	// Every request must carry a signature over the three headers, whatever
	// else it is doing. Checked here rather than in its own test because the
	// interesting failure is a request that forgets one, not a signature that
	// is wrong -- the published vectors above cover wrongness.
	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") ||
		!strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") ||
		r.Header.Get("X-Amz-Date") == "" || r.Header.Get("X-Amz-Content-Sha256") == "" {

		http.Error(w, "unsigned", http.StatusForbidden)

		return
	}

	q := r.URL.Query()

	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		fmt.Fprint(w, `<?xml version="1.0"?><InitiateMultipartUploadResult>`+
			`<Bucket>b</Bucket><Key>k</Key><UploadId>upload-1</UploadId>`+
			`</InitiateMultipartUploadResult>`)

	case r.Method == http.MethodPut:
		number := q.Get("partNumber")

		if number == f.failPart {
			http.Error(w, "no", http.StatusInternalServerError)

			return
		}

		n, _ := io.Copy(io.Discard, r.Body)

		time.Sleep(f.delay)

		f.mu.Lock()
		f.parts[number] = n
		f.mu.Unlock()

		w.Header().Set("ETag", `"deadbeef"`)

	case r.Method == http.MethodDelete:
		f.mu.Lock()
		f.aborted = true
		f.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func newFake(t *testing.T) (*fakeS3, Target) {
	t.Helper()

	f := &fakeS3{parts: map[string]int64{}}
	srv := httptest.NewServer(f)

	t.Cleanup(srv.Close)

	return f, Target{Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", Key: "k"}
}

func creds() Credentials {
	return Credentials{AccessKeyID: []byte("AKIDEXAMPLE"), SecretAccessKey: []byte("secret")}
}

// small keeps a test's probe to a few hundred kilobytes. The defaults are
// sized for a real connection and would push 64 MiB through a loopback server.
func small() Options {
	return Options{WarmUp: 64 << 10, Target: 10 * time.Millisecond, MinPart: 128 << 10, MaxPart: 256 << 10}
}

func TestAProbeUploadsAndThenThrowsItAway(t *testing.T) {
	f, target := newFake(t)

	got, err := Measure(context.Background(), target, creds(), small())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}

	if !f.aborted {
		t.Error("the upload was never aborted, so the probe left parts in somebody's bucket")
	}

	// The whole promise of this package: nothing is ever completed, so nothing
	// becomes an object.
	for _, call := range f.calls {
		if call == "POST /b/k?uploadId=upload-1" {
			t.Error("the multipart upload was COMPLETED; it must only ever be aborted")
		}
	}

	if f.parts["1"] != 64<<10 {
		t.Errorf("the warm-up part carried %d bytes, want %d", f.parts["1"], 64<<10)
	}

	if f.parts["2"] == 0 {
		t.Error("no timed part was uploaded")
	}

	if got.Sent != f.parts["2"] {
		t.Errorf("the result says %d bytes were sent and the endpoint received %d",
			got.Sent, f.parts["2"])
	}

	if got.BytesPerSecond <= 0 || got.MeasuredAt.IsZero() {
		t.Errorf("the measurement is not usable: %+v", got)
	}
}

// TestTheTimedPartIsSizedFromTheWarmUp checks the adaptive part of the probe:
// a slow connection must not be asked to send the maximum, and a fast one must
// not be measured over a part that was over before it started.
func TestTheTimedPartIsSizedFromTheWarmUp(t *testing.T) {
	f, target := newFake(t)

	// 64 KiB taking 50ms is about 1.3 MB/s. Eight seconds of that is ~10 MiB,
	// which the ceiling below holds at 2 MiB -- the point being that the size
	// is computed rather than constant.
	f.delay = 50 * time.Millisecond

	opts := small()
	opts.Target = 8 * time.Second
	opts.MinPart = 64 << 10
	opts.MaxPart = 2 << 20

	if _, err := Measure(context.Background(), target, creds(), opts); err != nil {
		t.Fatalf("Measure: %v", err)
	}

	if f.parts["2"] != 2<<20 {
		t.Errorf("the timed part was %d bytes; the ceiling is %d and the warm-up "+
			"rate asked for more", f.parts["2"], 2<<20)
	}
}

// TestAFailedTimedPartFallsBackToTheWarmUp is the case where the connection
// dropped halfway. The warm-up did reach the bucket, so there is a real if
// rougher number, and reporting nothing would be worse than reporting it.
func TestAFailedTimedPartFallsBackToTheWarmUp(t *testing.T) {
	f, target := newFake(t)
	f.failPart = "2"

	got, err := Measure(context.Background(), target, creds(), small())
	if err != nil {
		t.Fatalf("Measure returned an error although the warm-up succeeded: %v", err)
	}

	if got.Sent != 64<<10 {
		t.Errorf("the fallback reported %d bytes, want the warm-up's %d", got.Sent, 64<<10)
	}

	if !f.aborted {
		t.Error("a probe that failed halfway must still abort its upload")
	}
}

// TestACancelledProbeStillAborts is the reason abort runs on a context of its
// own. The path that reaches it most often is the one where the context passed
// in has just been cancelled.
func TestACancelledProbeStillAborts(t *testing.T) {
	f, target := newFake(t)
	f.delay = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := Measure(ctx, target, creds(), small()); err == nil {
		t.Fatal("a cancelled probe reported success")
	}

	if !f.aborted {
		t.Error("a cancelled probe left its upload behind")
	}
}

func TestSecondsEstimatesFromTheMeasuredRate(t *testing.T) {
	r := Result{BytesPerSecond: 1 << 20}

	if got := r.Seconds(10 << 20); got != 10*time.Second {
		t.Errorf("10 MiB at 1 MiB/s is %s, want 10s", got)
	}

	// An unmeasured rate estimates nothing rather than dividing by zero and
	// putting an infinity on the page.
	if got := (Result{}).Seconds(1 << 30); got != 0 {
		t.Errorf("an unmeasured rate estimated %s, want zero", got)
	}
}
