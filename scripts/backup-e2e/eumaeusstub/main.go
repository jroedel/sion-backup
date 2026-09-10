// Command eumaeusstub pretends to be Eumaeus, so that a real sion-backup can
// take a real backup to a real bucket without a real server.
//
// # Why this can be plain HTTP
//
// foundation/eumaeusapi refuses to send a machine token over anything but
// HTTPS — except to loopback, which it allows deliberately, because a server
// on the same machine has no wire to sniff. So this needs no certificate and
// no DNS trick: it listens on 127.0.0.1 and the machine's config.toml points
// at it. Everything the binary does above that line is unchanged.
//
// # What it decides
//
// The repository URL, which is the whole point. The client never chooses where
// its backups go — it asks, every run, and throws the answer away afterwards
// (see business/domain/credential/credentialbus). So this is the seam where a
// test bucket and a per-run prefix get injected, and the binary cannot tell
// the difference between this and the fleet's own server.
//
// # What it does not do
//
// It does not create the repository. Neither does the client: restic.Init
// exists in foundation/restic and nothing in the binary calls it, because
// provisioning a bucket and initialising a repository in it are the server's
// job. So the harness runs `restic init` itself, which is the one part of this
// that stands in for something Eumaeus does rather than something it serves.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:8088", "listen address; must be loopback")
		repoURL  = flag.String("repo", "", "restic repository URL to hand out")
		password = flag.String("password", "", "restic repository password (generated when empty)")
		keyID    = flag.String("key-id", "", "S3 access key id")
		secret   = flag.String("secret", "", "S3 secret access key")
		nodeID   = flag.String("node", "gate-machine", "node id to enrol as")
		journal  = flag.String("journal", "", "append every request to this file, as JSON lines")
		ready    = flag.String("ready", "", "touch this file once listening")
	)

	flag.Parse()

	if *repoURL == "" || *keyID == "" || *secret == "" {
		log.Fatal("eumaeusstub: -repo, -key-id and -secret are required")
	}

	pw := *password
	if pw == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			log.Fatalf("eumaeusstub: %v", err)
		}

		pw = hex.EncodeToString(raw)
	}

	s := &server{
		repo:     *repoURL,
		password: pw,
		keyID:    *keyID,
		secret:   *secret,
		node:     *nodeID,
		journal:  *journal,
	}

	mux := http.NewServeMux()

	const prefix = "/api/backup/v1"

	mux.HandleFunc(prefix+"/enrollments/claim", s.claim)
	mux.HandleFunc(prefix+"/machines/me/credentials", s.credentials)
	mux.HandleFunc(prefix+"/runs", s.accept("run"))
	mux.HandleFunc(prefix+"/diagnostics", s.accept("diagnostic"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record("unhandled", map[string]any{"method": r.Method, "path": r.URL.Path})
		log.Printf("eumaeusstub: 404 %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	// The password is printed once, on purpose: without it the repository this
	// run creates cannot be opened again by a person looking into a failure.
	// It is a throwaway for a throwaway bucket.
	log.Printf("eumaeusstub: %s -> %s (password %s)", *addr, s.repo, pw)

	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(pw), 0o600); err != nil {
			log.Fatalf("eumaeusstub: writing the ready file: %v", err)
		}
	}

	log.Fatal(http.ListenAndServe(*addr, mux))
}

// server holds what every response is built from.
type server struct {
	repo     string
	password string
	keyID    string
	secret   string
	node     string
	journal  string

	mu sync.Mutex
}

// record appends one line to the journal.
//
// The journal is how the harness asserts on what the machine SAID, not only on
// what the bucket contains. A run that backed up correctly and reported
// nothing is a machine that has gone silent on the dashboard, which is the
// failure this whole program exists to make visible.
func (s *server) record(kind string, body any) {
	if s.journal == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.journal, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("eumaeusstub: journal: %v", err)

		return
	}
	defer f.Close()

	line, err := json.Marshal(map[string]any{
		"kind": kind,
		"at":   time.Now().UTC().Format(time.RFC3339Nano),
		"body": body,
	})
	if err != nil {
		return
	}

	fmt.Fprintln(f, string(line))
}

// keyPair mirrors the wire form in eumaeuscreds.
type keyPair struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	var body map[string]any

	if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
		_ = json.Unmarshal(raw, &body)
	}

	s.record("claim", body)
	log.Printf("eumaeusstub: claim from %v", body["hostname"])

	resp := map[string]any{
		"machine_token": "gate-token-" + s.node,
		"node_id":       s.node,
		"owner": map[string]string{
			"name":  "Release Gate",
			"email": "gate@example.invalid",
		},
		"warn_after_hours": 48,
		"repository": map[string]string{
			"url":      s.repo,
			"provider": "wasabi",
			"region":   "test",
			"bucket":   "test",
		},
		"credentials": map[string]any{
			"restic_password": s.password,
			"machine":         keyPair{s.keyID, s.secret},
			// The restore key goes on the owner's printed card and is meant to
			// be read-only. There is one key in this harness, so the same pair
			// is returned -- which is fine for a throwaway bucket and would be
			// wrong anywhere else.
			"restore": keyPair{s.keyID, s.secret},
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *server) credentials(w http.ResponseWriter, r *http.Request) {
	// Every backup fetches these afresh, so this is the most-called endpoint
	// and the one whose failure stops the fleet. Counted in the journal so a
	// test can assert the client really does re-fetch rather than cache.
	s.record("credentials", map[string]any{"auth": r.Header.Get("Authorization") != ""})

	writeJSON(w, http.StatusOK, map[string]any{
		"credentials_version": 1,
		"repository":          map[string]string{"url": s.repo},
		"credentials": map[string]any{
			"restic_password": s.password,
			"machine":         keyPair{s.keyID, s.secret},
		},
	})
}

// accept records a posted body and answers 202, which is what the API asks
// for: a client retrying a report is a client spending its evening on
// something that is not a backup.
func (s *server) accept(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			_ = json.Unmarshal(raw, &body)
		}

		s.record(kind, body)

		if kind == "run" {
			log.Printf("eumaeusstub: run %v %v", body["phase"], body["outcome"])
		} else {
			log.Printf("eumaeusstub: %s %v", kind, body["kind"])
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("eumaeusstub: encoding: %v", err)
	}
}
