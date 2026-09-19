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
// It does not create the FIRST repository. The harness runs `restic init`
// itself for that, standing in for the provisioning step that is Eumaeus's
// job rather than anything it serves.
//
// A rotation's second bucket is different, and it is the one place a client
// may create a repository: `expect_empty` on the credential fetch says so, and
// this decides that from the three conditions in the specification — see
// rotation.go, which also holds the offer, the cutover, the release of the old
// bucket and the card. So a rotation run really does watch the binary create a
// repository, which is the behaviour with the worst failure mode in the whole
// program.
//
// # What is still fake, and worth remembering
//
// Everything about people, and one reading of the contract. A second bucket
// here is another prefix in the same bucket with the same key pair and the
// same password; the real server provisions storage, mints new keys and draws
// a new password. And this file agrees, by construction, with the reading of
// the specification held by whoever wrote it — which is why
// scripts/rotation-gate exists and runs the real server.
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
	"strings"
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

		// The two fields a bucket adopted from a legacy install carries.
		// Off by default, which is also what an unadopted machine looks
		// like — and what every machine adopted before Eumaeus grew these
		// fields still looks like, which is why the client treats their
		// absence as "ask restic" rather than as a denial.
		adopted   = flag.Bool("adopted", false, "answer the claim as an adopted repository")
		snapshots = flag.Int("snapshots", 0, "snapshots the adopted repository already holds")
		since     = flag.String("history-since", "", "history horizon, as 2006-01-02")
		journal   = flag.String("journal", "", "append every request to this file, as JSON lines")
		ready     = flag.String("ready", "", "touch this file once listening")
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
		password:  pw,
		keyID:     *keyID,
		secret:    *secret,
		node:      *nodeID,
		journal:   *journal,
		snapshots: *snapshots,
		since:     *since,
	}

	// The machine's first repository, active from the start. Every other row
	// arrives through POST /admin/offer, which is the harness standing in for
	// an administrator at /fleet.
	created := time.Now().UTC()
	if *since != "" {
		if parsed, err := time.Parse("2006-01-02", *since); err == nil {
			created = parsed
		}
	}

	s.repos = []*repo{{
		URL:       *repoURL,
		State:     stateActive,
		Bucket:    lastSegment(*repoURL),
		Region:    "test",
		CreatedAt: created,
		Adopted:   *adopted,
	}}

	mux := http.NewServeMux()

	const prefix = "/api/backup/v1"

	// All ten, because a client that asks what a deployment serves and is told
	// the truth is a client whose api.serves handling is being exercised
	// rather than assumed.
	mux.HandleFunc("GET "+prefix+"/machines/me", s.state)
	mux.HandleFunc("POST "+prefix+"/enrollments/claim", s.claim)
	mux.HandleFunc("GET "+prefix+"/machines/me/credentials", s.credentials)
	mux.HandleFunc("POST "+prefix+"/runs", s.accept("run"))
	mux.HandleFunc("POST "+prefix+"/diagnostics", s.accept("diagnostic"))
	mux.HandleFunc("POST "+prefix+"/machines/me/rotation-request", s.rotationRequest)
	mux.HandleFunc("POST "+prefix+"/machines/me/cutover", s.cutover)
	mux.HandleFunc("POST "+prefix+"/machines/me/old-bucket", s.oldBucket)
	mux.HandleFunc("POST "+prefix+"/machines/me/card-issued", s.cardIssued)

	// The administrator's half, deliberately off the versioned prefix so that
	// nothing here can be mistaken for something the client may call.
	mux.HandleFunc("POST /admin/offer", s.adminOffer)
	mux.HandleFunc("POST /admin/retire", s.adminRetire)
	mux.HandleFunc("POST /admin/point-at", s.adminPointAt)
	mux.HandleFunc("GET /admin/state", s.adminState)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record("unhandled", map[string]any{"method": r.Method, "path": r.URL.Path})
		log.Printf("eumaeusstub: 404 %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	// The password is printed once, on purpose: without it the repository this
	// run creates cannot be opened again by a person looking into a failure.
	// It is a throwaway for a throwaway bucket.
	log.Printf("eumaeusstub: %s -> %s (password %s)", *addr, *repoURL, pw)

	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(pw), 0o600); err != nil {
			log.Fatalf("eumaeusstub: writing the ready file: %v", err)
		}
	}

	log.Fatal(http.ListenAndServe(*addr, mux))
}

// server holds what every response is built from.
//
// One machine, because that is what a stub on a machine's own loopback is:
// every request is from the machine it is running beside, and there is no
// token to tell apart.
type server struct {
	password string
	keyID    string
	secret   string
	node     string
	journal  string

	// snapshots and since describe a repository taken over from a legacy
	// install rather than provisioned empty. See the -adopted flag; the
	// adopted flag itself lives on the repository row.
	snapshots int
	since     string

	// repos is every repository this machine has had, oldest first, and the
	// state machine in rotation.go is entirely about which one is selected.
	// rotationAsked is the open work item.
	repos         []*repo
	rotationAsked bool

	// mu guards the rows above. jmu guards the journal file and nothing else,
	// and the two are separate on purpose: every rotation handler records
	// while holding mu, so one mutex for both would deadlock on the first
	// request.
	mu  sync.Mutex
	jmu sync.Mutex
}

// repoURL is the repository the machine writes to now, for the log line and
// for the claim. Callers that need the row take the lock and use current().
func (s *server) repoURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cur := s.current(); cur != nil {
		return cur.URL
	}

	return ""
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

	s.jmu.Lock()
	defer s.jmu.Unlock()

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

	if where := legacyRepository(body); where != "" && !sameRepository(where, s.repoURL()) {
		// The refusal that exists for one mistake: a code that enrols this
		// machine against a bucket it has not been writing to. Answered here
		// so the client's half of it can be exercised without the real
		// server. The code is not consumed, which in a stub means nothing at
		// all is consumed.
		log.Printf("eumaeusstub: refusing a claim from a machine writing to %s", where)

		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "this machine backs up to " + where + ", and that code enrols it " +
				"against " + s.repoURL() + ". The code has not been used: adopt the bucket " +
				"the machine is already writing to, then present it again",
			"field": "legacy.repository_url",
		})

		return
	}

	resp := map[string]any{
		"machine_token": "gate-token-" + s.node,
		"node_id":       s.node,
		"owner": map[string]string{
			"name":  "Release Gate",
			"email": "gate@example.invalid",
		},
		"warn_after_hours": 48,
		"repository":       s.repository(),
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

// legacyRepository reads where the claiming machine says it has been backing
// up. Absent is not a mismatch: a client that found an install and could not
// open its repository says nothing here, and "I do not know" must not be read
// as "somewhere else".
func legacyRepository(body map[string]any) string {
	legacy, ok := body["legacy"].(map[string]any)
	if !ok {
		return ""
	}

	where, _ := legacy["repository_url"].(string)

	return where
}

// sameRepository compares two repository URLs the way the contract says to:
// surrounding space and trailing slashes ignored, case significant, because a
// provider where Bucket123 and bucket123 are two buckets is a provider where
// folding case accepts a claim against the wrong one.
func sameRepository(a, b string) bool {
	trim := func(s string) string {
		return strings.TrimRight(strings.TrimSpace(s), "/")
	}

	return trim(a) == trim(b)
}

// repository is the claim's repository block.
//
// The adoption fields are omitted rather than sent as false and zero, which is
// what the real server does and what the client depends on: absence means "ask
// restic", and a hard `"adopted": false` would be a denial the server is not
// in a position to make.
func (s *server) repository() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current()
	if cur == nil {
		return map[string]any{}
	}

	out := map[string]any{
		"url":      cur.URL,
		"provider": "wasabi",
		"region":   cur.Region,
		"bucket":   cur.Bucket,
	}

	if cur.Adopted {
		out["adopted"] = true
	}

	if s.snapshots > 0 {
		out["snapshots"] = s.snapshots
	}

	if s.since != "" {
		out["created_at"] = s.since + "T00:00:00Z"
	}

	return out
}

func (s *server) credentials(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current()
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this machine has no repository",
		})

		return
	}

	permitted := expectEmpty(cur)

	// Every backup fetches these afresh, so this is the most-called endpoint
	// and the one whose failure stops the fleet. Counted in the journal so a
	// test can assert the client really does re-fetch rather than cache — and
	// the permission is recorded with it, so the harness can prove the client
	// created a repository only on the fetch that allowed it.
	s.record("credentials", map[string]any{
		"auth":         r.Header.Get("Authorization") != "",
		"url":          cur.URL,
		"state":        cur.State,
		"expect_empty": permitted,
	})

	repository := map[string]any{
		"url":   cur.URL,
		"state": cur.State,
	}

	// Omitted when false, and that polarity is the contract rather than an
	// encoding detail: absence has to mean what it meant before the field
	// existed, which is "create nothing".
	if permitted {
		repository["expect_empty"] = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"credentials_version": 1,
		"repository":          repository,
		"credentials": map[string]any{
			"restic_password": s.password,
			"machine":         keyPair{s.keyID, s.secret},
			// One key pair in this harness, so the read-only card key is the
			// same pair. Fine for a throwaway bucket and wrong anywhere else.
			"restore": keyPair{s.keyID, s.secret},
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
			// Where the server's own evidence comes from. expect_empty's third
			// condition and the old-bucket refusal are both decided from what
			// the machine reported landing, not from anything it asserts about
			// itself — `seeding` on this event is deliberately read by nothing.
			s.noteRun(body)

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
