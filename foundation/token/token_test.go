package token_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/foundation/token"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token")

	if err := token.Save(path, "mt_7f3c9a1e"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := token.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got != "mt_7f3c9a1e" {
		t.Errorf("got %q", got)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := token.Load(filepath.Join(t.TempDir(), "absent"))

	if !errors.Is(err, token.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestAnEmptyFileIsNotEnrolled covers the interrupted-write case. A
// zero-length token must read as "never enrolled" rather than as a token that
// will fail authentication — those produce very different error messages, and
// only one of them sends somebody to the right place.
func TestAnEmptyFileIsNotEnrolled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")

	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := token.Load(path); !errors.Is(err, token.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

func TestSaveIsOwnerOnly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX modes are not how Windows answers this")
	}

	path := filepath.Join(t.TempDir(), "token")

	if err := token.Save(path, "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token has mode %o, want 600", perm)
	}
}

func TestSaveReplacesAndLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	for _, v := range []string{"first", "second"} {
		if err := token.Save(path, v); err != nil {
			t.Fatalf("Save %q: %v", v, err)
		}
	}

	got, err := token.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got != "second" {
		t.Errorf("got %q, want the replacement", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".token-") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestSaveRefusesEmpty(t *testing.T) {
	if err := token.Save(filepath.Join(t.TempDir(), "token"), ""); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")

	if err := token.Save(path, "v"); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		if err := token.Delete(path); err != nil {
			t.Fatalf("Delete %d: %v", i+1, err)
		}
	}

	if _, err := token.Load(path); !errors.Is(err, token.ErrNotEnrolled) {
		t.Errorf("after Delete: %v", err)
	}
}

func TestWipe(t *testing.T) {
	b := []byte("secret")
	token.Wipe(b)

	for i, c := range b {
		if c != 0 {
			t.Fatalf("byte %d survived: %q", i, c)
		}
	}
}
