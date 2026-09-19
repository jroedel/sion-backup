package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The rotation half of the stub: the four endpoints a machine calls about its
// own repository, the state call they are all reached through, and the small
// state machine behind them.
//
// # Why this is a state machine and not a set of canned replies
//
// Because the thing worth testing is whether the client's reading of the
// contract is right, and canned replies can only ever confirm the reading the
// person writing them already had. The three conditions behind `expect_empty`
// are implemented here from the specification rather than from the client's
// code, and the same goes for the refusals: the old bucket cannot be let go
// until a **verified** run has landed in the new one, and this decides that
// from the run events the machine itself reported.
//
// It is still a stub and it still agrees with one reading of the document.
// What catches a misreading of the document is scripts/rotation-gate, which
// runs the real server.
//
// # What it is allowed to skip
//
// Everything about people. The real server provisions storage, mints keys,
// draws a password and puts work items in front of an administrator. Here a
// second bucket is another prefix in the same test bucket with the same key
// pair and the same password, created by POST /admin/offer, because none of
// that is what the client's half is being asked about.

// Repository states, as the server spells them.
const (
	stateActive      = "active"
	stateOffered     = "offered"
	stateCuttingOver = "cutting-over"
	stateRetired     = "retired"
)

// repo is one row of the server's repositories table.
type repo struct {
	URL       string    `json:"url"`
	State     string    `json:"state"`
	Bucket    string    `json:"bucket"`
	Region    string    `json:"region"`
	CreatedAt time.Time `json:"created_at"`
	Adopted   bool      `json:"adopted,omitempty"`

	// OfferedAt is when the storage was created, which is when it became
	// available to the machine.
	OfferedAt time.Time `json:"offered_at,omitzero"`

	// CardIssuedAt is when the owner's card for THIS bucket was rendered.
	CardIssuedAt time.Time `json:"card_issued_at,omitzero"`

	// ReleaseAskedAt is when the machine first said it was finished with the
	// bucket it moved off. Kept on a retry rather than moved, because "when
	// did this machine say it was done" has to stay answerable.
	ReleaseAskedAt time.Time `json:"release_asked_at,omitzero"`

	// Snapshot and Verified are what the machine has reported landing here.
	// They are the evidence behind expect_empty's third condition and behind
	// the old-bucket refusal, and they come from the run events rather than
	// from anything the client asserts about itself.
	Snapshot bool `json:"snapshot"`
	Verified bool `json:"verified"`
}

// current is the repository the machine writes to.
//
// The whole state machine in one function, and it is the shape of the real
// server's query: a cutting-over row wins over an active one, and an offered
// row is never selected at all. Excluding `offered` is what makes provisioning
// an offer rather than an order — and preferring `cutting-over` is what sends
// the seeding run to the new bucket while the old row still exists as the
// fallback.
func (s *server) current() *repo {
	var active *repo

	for _, r := range s.repos {
		switch r.State {
		case stateCuttingOver:
			return r
		case stateActive:
			active = r
		}
	}

	return active
}

// offered is the newest bucket waiting to be accepted, or nil.
//
// Newest wins: a second offer is an administrator correcting the first — a
// wrong region, a typo in a bucket name — and the machine should be pointed at
// the correction.
func (s *server) offered() *repo {
	for i := len(s.repos) - 1; i >= 0; i-- {
		if s.repos[i].State == stateOffered {
			return s.repos[i]
		}
	}

	return nil
}

func (s *server) find(url string) *repo {
	for _, r := range s.repos {
		if sameRepository(r.URL, url) {
			return r
		}
	}

	return nil
}

// expectEmpty is the only thing this API ever says that permits a client to
// create a repository.
//
// All three conditions, written out from the specification rather than from
// the client:
//
//	the repository is cutting-over   — we deliberately sent it somewhere new
//	it was not adopted               — and somewhere new is not full of history
//	nothing has been written yet     — and the window closes when the seed lands
//
// The third is the one that is easy to leave out and the one that matters
// most. Nothing drives a repository out of cutting-over on its own, so without
// it every night of a stalled cutover would be a night on which an emptied
// bucket was silently recreated.
func expectEmpty(r *repo) bool {
	return r != nil && r.State == stateCuttingOver && !r.Adopted && !r.Snapshot
}

// cardState resolves the card across every bucket this machine has had, which
// is what makes `superseded` distinct from `never`.
//
// `never` — nobody has printed a card for the bucket the machine writes to
// now. This is what a machine reads immediately after a cutover, and while the
// old bucket's keys still work its card is the only way into the only bucket
// holding any history.
//
// `superseded` — the owner is holding paper that no longer opens anything,
// which can only be true once the old bucket has been retired. That is the
// whole reason the two words are worth telling apart.
func (s *server) cardState() (string, time.Time) {
	cur := s.current()

	if cur != nil && !cur.CardIssuedAt.IsZero() {
		return "issued", cur.CardIssuedAt
	}

	for _, r := range s.repos {
		if r.State == stateRetired && !r.CardIssuedAt.IsZero() {
			return "superseded", time.Time{}
		}
	}

	return "never", time.Time{}
}

// serves is what this deployment answers, so that a client asking rather than
// inferring gets a true answer. Ten routes, as the real server serves.
func serves() []string {
	return []string{
		"POST /enrollments/claim",
		"POST /diagnostics",
		"GET /machines/me",
		"GET /machines/me/credentials",
		"GET /machines/me/disclosures",
		"POST /runs",
		"POST /machines/me/rotation-request",
		"POST /machines/me/cutover",
		"POST /machines/me/old-bucket",
		"POST /machines/me/card-issued",
	}
}

// state answers GET /machines/me.
func (s *server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("state", map[string]any{"auth": r.Header.Get("Authorization") != ""})

	cur := s.current()
	if cur == nil {
		// Documented, and it does not mean what a 404 usually means: the token
		// is good and the machine has no repository.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this machine has no repository",
		})

		return
	}

	cardWord, issuedAt := s.cardState()

	card := map[string]any{"state": cardWord, "issued_at": nil}
	if !issuedAt.IsZero() {
		card["issued_at"] = issuedAt.UTC().Format(time.RFC3339)
	}

	body := map[string]any{
		"node_id": s.node,
		"owner": map[string]string{
			"name":  "Release Gate",
			"email": "gate@example.invalid",
		},
		"state":               "active",
		"paused_until":        nil,
		"warn_after_hours":    48,
		"repository":          repositoryState(cur),
		"credentials_version": 1,
		"card":                card,
		"server_time":         time.Now().UTC().Format(time.RFC3339),
		"api":                 map[string]any{"serves": serves()},
	}

	// Absent for almost every machine almost always, and absent rather than
	// null: a client that could not tell "no offer" from "an offer with empty
	// fields" would eventually accept the second.
	if o := s.offered(); o != nil {
		offer := map[string]any{
			"url":        o.URL,
			"provider":   "wasabi",
			"region":     o.Region,
			"bucket":     o.Bucket,
			"offered_at": o.OfferedAt.UTC().Format(time.RFC3339),
		}

		// Omitted when false, so an offer of ordinary empty storage carries no
		// adopted key at all.
		if o.Adopted {
			offer["adopted"] = true
		}

		body["offer"] = offer
	}

	writeJSON(w, http.StatusOK, body)
}

// repositoryState is the repository block on the state call.
func repositoryState(r *repo) map[string]any {
	out := map[string]any{
		"url":        r.URL,
		"state":      r.State,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
	}

	if r.Adopted {
		out["adopted"] = true
	}

	return out
}

// rotationRequest answers POST /machines/me/rotation-request.
//
// A work item and not a command, and there is deliberately no 403: the
// fleet-wide switch that used to refuse here is withdrawn, so every refusal
// below is about the fleet's state rather than about policy.
func (s *server) rotationRequest(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("rotation-request", body)

	cur := s.current()

	switch {
	case cur == nil:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this machine has no repository to rotate away from",
		})

	case s.rotationAsked:
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a rotation request is already open for this machine",
		})

	case cur.State == stateCuttingOver:
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this machine is already cutting over to a new bucket",
		})

	case s.offered() != nil:
		// The refusal worth telling apart. The work an administrator could do
		// has been done, and what is outstanding is the machine's own
		// decision, which no administrator can move.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a bucket has already been provisioned and is waiting for this machine",
		})

	default:
		s.rotationAsked = true
		log.Printf("eumaeusstub: rotation requested for %v", body["repository_url"])
		w.WriteHeader(http.StatusAccepted)
	}
}

// cutover answers POST /machines/me/cutover.
//
// The only thing in this program that starts a cutover, and reachable only
// from a machine's own token.
func (s *server) cutover(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	named, _ := body["repository_url"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("cutover", body)

	cur := s.current()
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this machine has no repository",
		})

		return
	}

	// Idempotency is checked BEFORE the offer is looked for. Otherwise a retry
	// would find nothing offered — because the offer is gone precisely because
	// the first call succeeded — and a lost acknowledgement would leave a
	// machine unable to agree it had been moved.
	if cur.State == stateCuttingOver && sameRepository(cur.URL, named) {
		writeJSON(w, http.StatusOK, map[string]any{
			"repository": map[string]string{"url": cur.URL, "state": stateCuttingOver},
		})

		return
	}

	offer := s.offered()
	if offer == nil {
		// 409 and not 404: the machine and the endpoint both exist, and a 404
		// here would be indistinguishable from the route not being served.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "nothing has been offered to this machine",
		})

		return
	}

	if !sameRepository(offer.URL, named) {
		// The sentence names both, because the difference is usually one word
		// of a bucket name.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": fmt.Sprintf("this machine is being offered %s, and the cutover named %s; "+
				"read the current offer and accept that", offer.URL, named),
		})

		return
	}

	offer.State = stateCuttingOver
	s.rotationAsked = false

	log.Printf("eumaeusstub: cutting over to %s", offer.URL)

	writeJSON(w, http.StatusOK, map[string]any{
		"repository": map[string]string{"url": offer.URL, "state": stateCuttingOver},
	})
}

// oldBucket answers POST /machines/me/old-bucket.
//
// It releases nothing. The old bucket stays live, stays readable and keeps its
// password; this writes one timestamp, which puts it in front of a person.
func (s *server) oldBucket(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	named, _ := body["repository_url"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("old-bucket", body)

	cur := s.current()
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this machine has no repository",
		})

		return
	}

	if cur.State != stateCuttingOver {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this machine is not being moved onto a new bucket",
		})

		return
	}

	// The URL named is the bucket the machine is on NOW, not the one being let
	// go. A client that has lost its state and is somehow still writing to the
	// old bucket names that URL, does not match, and is refused rather than
	// releasing the bucket it is actually using.
	if !sameRepository(cur.URL, named) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": fmt.Sprintf("this machine is being moved onto %s, and the request named "+
				"%s; name the bucket it is writing to now", cur.URL, named),
		})

		return
	}

	// Nothing verified has landed yet, so the old bucket is still the only
	// readable copy of this machine's history. Clears by itself.
	if !cur.Verified {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "nothing verified has landed in the new bucket yet",
		})

		return
	}

	old := s.previousTo(cur)
	if old == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "there is no earlier bucket to let go of",
		})

		return
	}

	// Kept on a retry rather than moved: release_asked_at is the date of the
	// FIRST call, and a retry that moved it would make "when did this machine
	// say it was done" unanswerable for the person about to act on it.
	if old.ReleaseAskedAt.IsZero() {
		old.ReleaseAskedAt = time.Now().UTC()
		log.Printf("eumaeusstub: the machine is finished with %s", old.URL)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bucket":           old.Bucket,
		"url":              old.URL,
		"release_asked_at": old.ReleaseAskedAt.Format(time.RFC3339),
	})
}

// previousTo is the bucket the machine is moving off, which is the newest
// non-retired row older than the one it is moving onto.
func (s *server) previousTo(cur *repo) *repo {
	var prev *repo

	for _, r := range s.repos {
		if r == cur || r.State == stateRetired || r.State == stateOffered {
			continue
		}

		prev = r
	}

	return prev
}

// cardIssued answers POST /machines/me/card-issued.
func (s *server) cardIssued(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	named, _ := body["repository_url"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.record("card-issued", body)

	cur := s.current()

	// The card must name the repository the machine writes to now, so that a
	// card printed for a superseded bucket cannot mark the current one as
	// covered.
	if cur == nil || !sameRepository(cur.URL, named) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "that card does not name this machine's current repository",
		})

		return
	}

	cur.CardIssuedAt = time.Now().UTC()
	log.Printf("eumaeusstub: a card was printed for %s", cur.URL)

	w.WriteHeader(http.StatusNoContent)
}

// noteRun records what a run event says landed where.
//
// This is where expect_empty's third condition and the old-bucket refusal get
// their evidence, and it is deliberately taken from the run event rather than
// from anything the client asserts about itself. `seeding` on that event is
// read by nothing here, on purpose: a flag a machine sets about itself must
// not be able to decide what the server permits it to create.
func (s *server) noteRun(body map[string]any) {
	if phase, _ := body["phase"].(string); phase != "finished" {
		return
	}

	snapshot, _ := body["snapshot_id"].(string)
	if snapshot == "" {
		return
	}

	url, _ := body["repository_url"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.find(url)
	if r == nil {
		return
	}

	r.Snapshot = true

	if verified, _ := body["verified"].(bool); verified {
		r.Verified = true
	}
}

// --- the administrator's half, which is not part of the API ---------------
//
// Deliberately off the versioned prefix, so that nothing here can be mistaken
// for something the client is entitled to call. This is the harness standing
// in for a person at /fleet.

// adminOffer provisions a second bucket and offers it.
func (s *server) adminOffer(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	url, _ := body["url"].(string)
	if url == "" {
		http.Error(w, "an offer needs a url", http.StatusBadRequest)

		return
	}

	adopted, _ := body["adopted"].(bool)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()

	s.repos = append(s.repos, &repo{
		URL:       url,
		State:     stateOffered,
		Bucket:    lastSegment(url),
		Region:    "test",
		CreatedAt: now,
		OfferedAt: now,
		Adopted:   adopted,
	})

	log.Printf("eumaeusstub: offering %s (adopted=%v)", url, adopted)

	writeJSON(w, http.StatusOK, map[string]any{"offered": url})
}

// adminRetire is the administrator's step 9: the superseded row goes to
// retired and the successor is promoted to active, in one unit.
//
// Promotion in the same unit is not an optimisation. Leaving the successor
// cutting-over would refuse every future rotation request from this machine
// for ever, which is a thing a harness ought to be able to notice.
func (s *server) adminRetire(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current()
	if cur == nil || cur.State != stateCuttingOver {
		http.Error(w, "nothing is cutting over", http.StatusConflict)

		return
	}

	old := s.previousTo(cur)
	if old == nil {
		http.Error(w, "there is no earlier bucket", http.StatusConflict)

		return
	}

	old.State = stateRetired
	cur.State = stateActive

	log.Printf("eumaeusstub: retired %s and promoted %s", old.URL, cur.URL)

	writeJSON(w, http.StatusOK, map[string]any{"retired": old.URL, "active": cur.URL})
}

// adminPointAt moves the machine's ACTIVE repository to a URL with nothing at
// it, leaving its state alone.
//
// There is no such operation on the real server, and that is the point: this
// is the harness manufacturing the situation the client's whole doctrine
// exists for. A machine is handed a URL, follows it, and finds nothing — which
// on the backup path means either the URL is wrong or the bucket has been
// emptied, and in neither case may anything be created. The only correct
// outcome is a loud failure.
//
// It backs the one assertion in this harness whose failure would mean a
// machine can silently start a brand-new empty backup and report success.
//
// # Why it refuses during a cutover
//
// Because the first version of this did not, and the assertion it backs
// quietly stopped testing anything. Called while the machine was still
// cutting over, it moved the `cutting-over` row and cleared the evidence that
// something had landed there — which is exactly the shape of a bucket the
// server means to send a machine to, so `expect_empty` came back true, the
// client created the repository, and the harness reported the program's
// correct behaviour as the catastrophic one.
//
// The refusal is the fix rather than a note in the script, because a harness
// whose safety depends on being called in the right order is a harness that
// will one day be called in the wrong one.
func (s *server) adminPointAt(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	url, _ := body["url"].(string)
	if url == "" {
		http.Error(w, "point-at needs a url", http.StatusBadRequest)

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.current()

	switch {
	case cur == nil:
		http.Error(w, "this machine has no repository", http.StatusConflict)

		return

	case cur.State != stateActive:
		http.Error(w, "point-at is for a machine in its steady state; this one is "+
			cur.State+", where an empty bucket may be exactly what the server meant",
			http.StatusConflict)

		return
	}

	was := cur.URL
	cur.URL = url
	cur.Bucket = lastSegment(url)

	// The evidence resets with the URL: nothing has landed at the new one. It
	// cannot make expect_empty true — an `active` repository never is, and the
	// refusal above is what keeps that so — but a row claiming a snapshot it
	// does not have would be a lie some later assertion might rest on.
	cur.Snapshot = false
	cur.Verified = false

	log.Printf("eumaeusstub: pointing the machine at %s (was %s), still %s", url, was, cur.State)

	writeJSON(w, http.StatusOK, map[string]any{"url": url, "state": cur.State})
}

// adminState dumps everything, so the harness can assert on the server's view
// rather than only on the machine's.
func (s *server) adminState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows := make([]repo, 0, len(s.repos))
	for _, r := range s.repos {
		rows = append(rows, *r)
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })

	cardWord, issuedAt := s.cardState()

	var currentURL string
	if cur := s.current(); cur != nil {
		currentURL = cur.URL
		_ = expectEmpty(cur)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repositories":   rows,
		"current":        currentURL,
		"expect_empty":   expectEmpty(s.current()),
		"card_state":     cardWord,
		"card_issued_at": issuedAt,
		"rotation_asked": s.rotationAsked,
		"offered":        offeredURL(s.offered()),
	})
}

func offeredURL(r *repo) string {
	if r == nil {
		return ""
	}

	return r.URL
}

func lastSegment(url string) string {
	parts := strings.Split(strings.TrimRight(url, "/"), "/")

	return parts[len(parts)-1]
}

// readBody decodes a JSON body, or returns an empty map. Every caller records
// what it got, so a malformed body shows up in the journal rather than as a
// silent zero value.
func readBody(r *http.Request) map[string]any {
	var body map[string]any

	if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
		_ = json.Unmarshal(raw, &body)
	}

	if body == nil {
		body = map[string]any{}
	}

	return body
}
