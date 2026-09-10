// Command releaseserver pretends to be GitHub, so that a real sion-backup
// binary can update itself without a real release existing.
//
// # Why it serves TLS on the real hostnames
//
// foundation/selfupdate reaches api.github.com over HTTPS and has no seam for
// pointing it somewhere else — deliberately. A configurable update host is a
// configurable place to be handed a binary, and on a Windows machine where the
// scheduled task runs elevated and the signed-in user is not an administrator,
// an environment variable that redirects the update source is a privilege
// escalation with a hash check that verifies the attacker's own hash.
//
// So this does not ask the program to be testable. It generates a CA, signs a
// certificate for api.github.com and the download host, and the harness puts
// the CA in the container's trust store and the server's address in its
// /etc/hosts. The binary under test is the release binary, unmodified, doing a
// real DNS lookup, a real TLS handshake and a real download. What it cannot
// tell is that the internet is one process on the other side of a bridge.
//
// # Faults
//
// The point is not the happy path, which the unit tests already cover with an
// httptest server. The point is that every way a release can be wrong ends
// with the machine still running the binary it started with. -fault names one;
// see faultUsage for the list and what each is for.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const faultUsage = `which way this release is broken:

  none          a correct release; the happy path
  wrong-hash    the binary is fine, SHA256SUMS lies about it
  truncated     half the binary, so the hash guard meets a real short read
  corrupt-mid   one flipped byte in the middle of an otherwise correct download
  no-sums       a release that publishes no SHA256SUMS at all
  no-asset      a release with no binary for this platform
  prerelease    /latest answers with prerelease: true
  draft         /latest answers with draft: true
  rate-limited  403 with GitHub's rate-limit body, as a shared office address gets
  slow          the binary at a trickle, to meet the download timeout
  hangup        the connection closed part-way through the body

A release whose binary reports a different version from its tag needs no fault:
the harness offers an asset stamped with the wrong version, and the smoke test
in foundation/selfupdate is what has to catch it.
`

func main() {
	var (
		addr     = flag.String("addr", ":443", "listen address")
		repo     = flag.String("repo", "jroedel/sion-backup", "owner/name this pretends to be")
		tag      = flag.String("tag", "v9.9.9", "the release tag to offer")
		assetDir = flag.String("assets", "", "directory holding sion-backup-<goos>-<goarch> binaries")
		hosts    = flag.String("hosts", "api.github.com,objects.githubusercontent.com,github.com", "comma-separated names the certificate covers")
		caOut    = flag.String("ca-out", "", "write the generated CA certificate here (PEM)")
		fault    = flag.String("fault", "none", faultUsage)
		ready    = flag.String("ready", "", "touch this file once listening")
	)

	flag.Parse()

	if *assetDir == "" {
		log.Fatal("releaseserver: -assets is required")
	}

	srv, err := newServer(*repo, *tag, *assetDir, *fault)
	if err != nil {
		log.Fatalf("releaseserver: %v", err)
	}

	cert, caPEM, err := selfSigned(strings.Split(*hosts, ","))
	if err != nil {
		log.Fatalf("releaseserver: certificate: %v", err)
	}

	if *caOut != "" {
		if err := os.WriteFile(*caOut, caPEM, 0o644); err != nil {
			log.Fatalf("releaseserver: writing the CA: %v", err)
		}
	}

	ln, err := tls.Listen("tcp", *addr, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		log.Fatalf("releaseserver: listen: %v", err)
	}

	log.Printf("releaseserver: %s serving %s %s (fault: %s)", ln.Addr(), *repo, *tag, *fault)

	// Touched last, so a harness that waits on this file is waiting for a
	// server that can actually answer rather than for a process that exists.
	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(ln.Addr().String()), 0o644); err != nil {
			log.Fatalf("releaseserver: writing the ready file: %v", err)
		}
	}

	log.Fatal(http.Serve(ln, srv))
}

// server answers the two requests selfupdate makes, plus the download.
type server struct {
	repo  string
	tag   string
	dir   string
	fault string

	mux *http.ServeMux
}

func newServer(repo, tag, dir, fault string) (*server, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("assets: %w", err)
	}

	s := &server{repo: repo, tag: tag, dir: dir, fault: fault, mux: http.NewServeMux()}

	s.mux.HandleFunc("/repos/"+repo+"/releases/latest", s.latest)
	s.mux.HandleFunc("/download/", s.download)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("releaseserver: 404 %s", r.URL.Path)
		http.NotFound(w, r)
	})

	return s, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("releaseserver: %s %s", r.Method, r.URL.Path)

	// Before routing, because a rate-limited address is rate-limited for the
	// release JSON and the checksum file alike — which is the case that
	// matters, since it is the one that turns into a report from every machine
	// in a building at once.
	if s.fault == "rate-limited" {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for 203.0.113.7.",`+
			`"documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"}`)

		return
	}

	s.mux.ServeHTTP(w, r)
}

// asset is one file in the release, as the API describes it.
type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// latest answers the release JSON.
func (s *server) latest(w http.ResponseWriter, r *http.Request) {
	base := "https://objects.githubusercontent.com/download/" + s.tag

	body := struct {
		TagName    string  `json:"tag_name"`
		Draft      bool    `json:"draft"`
		Prerelease bool    `json:"prerelease"`
		Assets     []asset `json:"assets"`
	}{
		TagName:    s.tag,
		Draft:      s.fault == "draft",
		Prerelease: s.fault == "prerelease",
	}

	names, err := os.ReadDir(s.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	for _, e := range names {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "sion-backup-") {
			continue
		}

		if s.fault == "no-asset" {
			continue
		}

		body.Assets = append(body.Assets, asset{Name: e.Name(), URL: base + "/" + e.Name()})
	}

	if s.fault != "no-sums" {
		body.Assets = append(body.Assets, asset{Name: "SHA256SUMS", URL: base + "/SHA256SUMS"})
	}

	// Shipped by the real release, and listed here so that a harness which
	// diffs this JSON against a real one sees the same shape.
	body.Assets = append(body.Assets, asset{Name: "restic.pin", URL: base + "/restic.pin"})

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("releaseserver: encoding the release: %v", err)
	}
}

// download serves an asset, or the checksum file computed over the others.
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Path)

	if name == "SHA256SUMS" {
		s.sums(w)

		return
	}

	// Base, so that a path from the request can never leave the asset
	// directory. It is a test server, but a test server with a directory
	// traversal in it is still a directory traversal.
	path := filepath.Join(s.dir, name)

	blob, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	switch s.fault {
	case "truncated":
		// Content-Length says the whole thing and the body is half of it. The
		// client sees an unexpected EOF, which is the shape a download killed
		// by a dropped link actually has.
		w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
		w.Write(blob[:len(blob)/2])

		return

	case "corrupt-mid":
		blob[len(blob)/2] ^= 0xff

	case "slow":
		// Slower than the download timeout allows for a file this size, in
		// small writes, so the failure is a timeout mid-transfer rather than a
		// refused connection.
		flusher, _ := w.(http.Flusher)

		for i := 0; i < len(blob); i += 4096 {
			end := min(i+4096, len(blob))

			w.Write(blob[i:end])

			if flusher != nil {
				flusher.Flush()
			}

			time.Sleep(200 * time.Millisecond)
		}

		return

	case "hangup":
		w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
		w.Write(blob[:len(blob)/2])

		// Take the connection away rather than closing the body politely: a
		// client that gets a clean EOF and a client that gets a reset are
		// distinguishable, and the second is what a lost link looks like.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					tcp.SetLinger(0)
				}

				conn.Close()
			}
		}

		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(blob)
}

// sums writes the checksum file in sha256sum's own format.
func (s *server) sums(w http.ResponseWriter) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	var out strings.Builder

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "sion-backup-") {
			continue
		}

		blob, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}

		sum := sha256.Sum256(blob)
		hexsum := hex.EncodeToString(sum[:])

		if s.fault == "wrong-hash" {
			// A plausible wrong answer rather than an obviously bogus one: the
			// message the program prints on a mismatch is read by a person,
			// and this is the harness's chance to see it as they would.
			hexsum = strings.Repeat("de", 32)
		}

		// Two spaces, as sha256sum writes it and as `make checksums` produces.
		fmt.Fprintf(&out, "%s  %s\n", hexsum, e.Name())
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, out.String())
}

// selfSigned makes a CA and one leaf certificate covering hosts.
//
// A CA rather than a bare self-signed leaf, because the container trusts the
// CA and the harness may want a second server later — a staged rollout
// rehearsal needs two — without reprovisioning trust.
func selfSigned(hosts []string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sion-backup selfupdate test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}
