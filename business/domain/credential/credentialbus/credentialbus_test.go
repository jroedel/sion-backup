package credentialbus_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
)

const repoURL = "s3:https://s3.us-central-1.wasabisys.com/example-node-bucket"

// fakeSource stands in for Eumaeus.
type fakeSource struct {
	set   credentialbus.Set
	err   error
	calls int
}

func (f *fakeSource) Fetch(context.Context) (credentialbus.Set, error) {
	f.calls++

	if f.err != nil {
		return credentialbus.Set{}, f.err
	}

	// A copy, because ForRun's caller wipes what it is given and a second call
	// must not hand back zeroed bytes. Eumaeus over HTTP behaves this way; a
	// source that shared one backing array would pass the tests and fail in
	// production on the second run.
	c := f.set.Credentials

	return credentialbus.Set{
		RepositoryURL: f.set.RepositoryURL,
		Version:       f.set.Version,
		Credentials: credentialbus.Credentials{
			AccessKeyID:     append([]byte(nil), c.AccessKeyID...),
			SecretAccessKey: append([]byte(nil), c.SecretAccessKey...),
			ResticPassword:  append([]byte(nil), c.ResticPassword...),
		},
	}, nil
}

func good() *fakeSource {
	return &fakeSource{set: credentialbus.Set{
		RepositoryURL: repoURL,
		Version:       3,
		Credentials: credentialbus.Credentials{
			AccessKeyID:     []byte("AKIAEXAMPLEACCESSKEY"),
			SecretAccessKey: []byte("s3-secret"),
			ResticPassword:  []byte("repo-password"),
		},
	}}
}

func TestForRun(t *testing.T) {
	src := good()

	set, err := credentialbus.NewBusiness(src).ForRun(context.Background())
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}
	defer set.Wipe()

	if set.RepositoryURL != repoURL {
		t.Errorf("repository %q", set.RepositoryURL)
	}

	if string(set.Credentials.ResticPassword) != "repo-password" {
		t.Errorf("password %q", set.Credentials.ResticPassword)
	}

	if set.Version != 3 {
		t.Errorf("version %d, want 3", set.Version)
	}
}

// TestEveryRunFetches is the property this package exists for. Nothing is
// cached, so a second backup asks the server again — which is what makes
// revoking a token sufficient to cut a machine off.
func TestEveryRunFetches(t *testing.T) {
	src := good()
	bus := credentialbus.NewBusiness(src)

	for i := range 3 {
		set, err := bus.ForRun(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}

		if string(set.Credentials.ResticPassword) != "repo-password" {
			t.Fatalf("run %d got %q", i+1, set.Credentials.ResticPassword)
		}

		set.Wipe()
	}

	if src.calls != 3 {
		t.Errorf("the source was asked %d times for 3 runs; something is caching", src.calls)
	}
}

// TestIncompleteIsRefused: a partial record on the server must fail here, with
// a message about the server. Passed through to restic it would surface as
// "unable to open repository", sending somebody to look at the bucket.
func TestIncompleteIsRefused(t *testing.T) {
	cases := map[string]credentialbus.Credentials{
		"no password": {AccessKeyID: []byte("a"), SecretAccessKey: []byte("b")},
		"no key id":   {SecretAccessKey: []byte("b"), ResticPassword: []byte("p")},
		"no secret":   {AccessKeyID: []byte("a"), ResticPassword: []byte("p")},
	}

	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			src := &fakeSource{set: credentialbus.Set{RepositoryURL: repoURL, Credentials: creds}}

			_, err := credentialbus.NewBusiness(src).ForRun(context.Background())
			if err == nil {
				t.Fatal("an incomplete credential set was accepted")
			}

			if !strings.Contains(err.Error(), "server") {
				t.Errorf("the message does not point at the server: %v", err)
			}
		})
	}
}

func TestMissingRepositoryURLIsRefused(t *testing.T) {
	src := good()
	src.set.RepositoryURL = ""

	if _, err := credentialbus.NewBusiness(src).ForRun(context.Background()); err == nil {
		t.Fatal("credentials with no repository URL were accepted")
	}
}

func TestUnenrolledMachine(t *testing.T) {
	bus := credentialbus.NewBusiness(nil)

	if bus.Enrolled() {
		t.Error("a machine with no source reports itself enrolled")
	}

	if _, err := bus.ForRun(context.Background()); !errors.Is(err, credentialbus.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestUnauthorisedIsDistinguishable lets the daemon stop retrying and the
// status page say something true, instead of showing a connection error to
// somebody whose machine was deliberately cut off.
func TestUnauthorisedIsDistinguishable(t *testing.T) {
	src := &fakeSource{err: &credentialbus.Unauthorised{Err: errors.New("401")}}

	_, err := credentialbus.NewBusiness(src).ForRun(context.Background())

	var revoked *credentialbus.Unauthorised
	if !errors.As(err, &revoked) {
		t.Fatalf("got %v, want an Unauthorised", err)
	}

	if !strings.Contains(err.Error(), "de-enrolled") {
		t.Errorf("the message does not explain what happened: %v", err)
	}
}

func TestRepositoryCarriesEverythingResticNeeds(t *testing.T) {
	c := credentialbus.Credentials{
		AccessKeyID:     []byte("id"),
		SecretAccessKey: []byte("secret"),
		ResticPassword:  []byte("password"),
	}

	repo := c.Repository(repoURL, 16, 4)

	if err := repo.Validate(); err != nil {
		t.Fatalf("the assembled repository is not usable: %v", err)
	}

	if repo.PackSizeMiB != 16 || repo.ReadConcurrency != 4 {
		t.Errorf("tuning was dropped: %+v", repo)
	}
}

func TestWipe(t *testing.T) {
	set := credentialbus.Set{Credentials: credentialbus.Credentials{
		AccessKeyID:     []byte("id"),
		SecretAccessKey: []byte("secret"),
		ResticPassword:  []byte("password"),
	}}

	set.Wipe()

	for _, b := range [][]byte{
		set.Credentials.AccessKeyID,
		set.Credentials.SecretAccessKey,
		set.Credentials.ResticPassword,
	} {
		for _, v := range b {
			if v != 0 {
				t.Fatalf("a secret survived Wipe: %q", b)
			}
		}
	}
}
