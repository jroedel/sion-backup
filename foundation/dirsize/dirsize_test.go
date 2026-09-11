package dirsize_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jroedel/sion-backup/foundation/dirsize"
)

// tree builds a directory of files whose sizes are known, so that a walk's
// arithmetic can be checked against something rather than against itself.
func tree(t *testing.T, files map[string]int) string {
	t.Helper()

	root := t.TempDir()

	for name, size := range files {
		path := filepath.Join(root, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func TestAWalkAddsUpTheFilesItFinds(t *testing.T) {
	root := tree(t, map[string]int{
		"documents/report.odt":      1000,
		"documents/old/notes.txt":   200,
		"pictures/holiday/one.jpeg": 3000,
	})

	got := dirsize.Walk(context.Background(), []string{root}, dirsize.Options{}, nil)

	if got.Bytes != 4200 {
		t.Errorf("%d bytes, want 4200", got.Bytes)
	}

	if got.Files != 3 {
		t.Errorf("%d files, want 3", got.Files)
	}

	if got.Partial {
		t.Error("a complete walk reported itself partial")
	}
}

// TestARootThatIsNotThereIsNotAFailure. A work machine with no ~/Videos is the
// ordinary case, and it must not turn up as an unreadable folder on a page
// whose whole job is to look trustworthy.
func TestARootThatIsNotThereIsNotAFailure(t *testing.T) {
	root := tree(t, map[string]int{"a.txt": 100})

	got := dirsize.Walk(context.Background(),
		[]string{root, filepath.Join(root, "nope")}, dirsize.Options{}, nil)

	switch {
	case got.Bytes != 100:
		t.Errorf("%d bytes, want 100", got.Bytes)
	case got.Unreadable != 0:
		t.Errorf("%d unreadable, want none: a folder that does not exist is not an error",
			got.Unreadable)
	}
}

func TestTheExcludesLeaveOutWhatRestticWould(t *testing.T) {
	root := tree(t, map[string]int{
		"work/report.odt":             1000,
		"work/project/node_modules/x": 5000,
		"work/project/src/main.go":    100,
		"downloads/ubuntu.iso":        9000,
		"cache/thumbnails/a.png":      700,
	})

	for _, c := range []struct {
		name     string
		excludes []string
		want     int64
	}{
		{
			name: "nothing excluded",
			want: 15800,
		},
		{
			// A bare name matches a directory component anywhere, and prunes
			// the whole subtree rather than each file in it.
			name:     "a bare directory name",
			excludes: []string{"node_modules"},
			want:     10800,
		},
		{
			name:     "a glob on the file name",
			excludes: []string{"*.iso"},
			want:     6800,
		},
		{
			name:     "an absolute path",
			excludes: []string{filepath.Join(root, "cache")},
			want:     15100,
		},
		{
			name:     "a relative path with a separator in it",
			excludes: []string{filepath.Join("cache", "thumbnails")},
			want:     15100,
		},
		{
			name:     "several at once",
			excludes: []string{"node_modules", "*.iso", filepath.Join(root, "cache")},
			want:     1100,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := dirsize.Walk(context.Background(), []string{root},
				dirsize.Options{Excludes: c.excludes}, nil)

			if got.Bytes != c.want {
				t.Errorf("%d bytes, want %d", got.Bytes, c.want)
			}
		})
	}
}

// TestTheSizeLimitSkipsBigFilesAndCountsThem mirrors restic's
// --exclude-larger-than, which is the one exclusion that cannot be written as
// a pattern.
func TestTheSizeLimitSkipsBigFilesAndCountsThem(t *testing.T) {
	root := tree(t, map[string]int{
		"small.txt": 100,
		"big.img":   9000,
	})

	got := dirsize.Walk(context.Background(), []string{root},
		dirsize.Options{LargerThan: 5000}, nil)

	switch {
	case got.Bytes != 100:
		t.Errorf("%d bytes, want 100", got.Bytes)
	case got.Files != 1:
		t.Errorf("%d files, want 1", got.Files)
	case got.Excluded != 1:
		t.Errorf("%d excluded, want 1", got.Excluded)
	}
}

// TestASymlinkIsNotFollowed. Following them is how a walk of a home directory
// counts the whole filesystem twice, or loops.
func TestASymlinkIsNotFollowed(t *testing.T) {
	root := tree(t, map[string]int{"real/file.txt": 500})

	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "loop")); err != nil {
		t.Skipf("this platform will not make a symlink here: %v", err)
	}

	got := dirsize.Walk(context.Background(), []string{root}, dirsize.Options{}, nil)

	if got.Bytes != 500 {
		t.Errorf("%d bytes, want 500 — the linked folder was counted twice", got.Bytes)
	}
}

// TestAnExhaustedWalkSaysSo: the result is a floor, not a total, and every
// caller renders it as "more than".
func TestAnExhaustedWalkSaysSo(t *testing.T) {
	root := tree(t, map[string]int{
		"a.txt": 10, "b.txt": 10, "c.txt": 10, "d.txt": 10, "e.txt": 10,
	})

	got := dirsize.Walk(context.Background(), []string{root},
		dirsize.Options{MaxEntries: 3}, nil)

	if !got.Partial {
		t.Error("a walk that ran out of budget did not report itself partial")
	}

	if got.Bytes == 0 || got.Bytes >= 50 {
		t.Errorf("%d bytes: a partial walk should have found some but not all", got.Bytes)
	}
}

// TestACancelledWalkStopsAndSaysSo.
func TestACancelledWalkStopsAndSaysSo(t *testing.T) {
	root := tree(t, map[string]int{"a.txt": 10, "b.txt": 10})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := dirsize.Walk(ctx, []string{root}, dirsize.Options{}, nil)

	if !got.Partial {
		t.Error("a cancelled walk did not report itself partial")
	}
}
