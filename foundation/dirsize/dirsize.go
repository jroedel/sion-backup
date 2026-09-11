// Package dirsize answers "roughly how big is this, if we backed it up".
//
// It exists for one sentence on the setup page — "your home folder, about
// 47 GB" — and the whole design follows from what that sentence is for. It is
// shown to somebody deciding whether to back up their documents or their whole
// home directory, and then multiplied by a measured upload rate to tell them
// how long the first backup will take. A figure that is a few percent out
// changes nothing about that decision. A figure that takes four minutes to
// appear means they have already clicked something else.
//
// So: an estimate, said to be one, produced while they watch.
//
// # What it is not
//
// It is not restic. restic deduplicates, compresses, and skips what it already
// has, so the bytes that actually go up the wire on a first run are fewer than
// this — usually by a third or better on a home directory full of documents,
// and by almost nothing on a folder of photographs. Nothing here tries to
// model that; the setup page says the estimate is an upper bound and why.
//
// Nor is the exclude matching restic's. See [Excludes].
package dirsize

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Result is what a walk found.
type Result struct {
	// Bytes is the total size of the regular files that would be backed up.
	Bytes int64

	// Files is how many there are.
	Files int64

	// Excluded is how many entries the exclude patterns kept out, counting a
	// pruned directory as one. Shown on the setup page so that turning the
	// "leave out the usual junk" checkbox on is visibly doing something.
	Excluded int64

	// Unreadable is how many directories could not be opened.
	//
	// Reported rather than swallowed, because on a machine where this is
	// large the estimate is not a small underestimate — it is an estimate of a
	// different, much smaller backup, and the person reading the page is about
	// to choose based on it.
	Unreadable int64

	// Took is how long the walk ran for.
	Took time.Duration

	// Partial reports a walk that stopped early: the context was cancelled,
	// or the entry budget ran out. What is in the result is a floor, not a
	// total, and every caller renders it as "more than".
	Partial bool
}

// Options shape a walk.
type Options struct {
	// Excludes are patterns to leave out, in the approximate dialect
	// described on [Excludes].
	Excludes []string

	// LargerThan skips regular files at or above this many bytes, matching
	// restic's --exclude-larger-than. Zero means no limit.
	LargerThan int64

	// MaxEntries bounds the walk. Zero uses [DefaultMaxEntries].
	//
	// The bound is on entries visited rather than on time, because a time
	// limit produces a different answer on the same machine depending on what
	// else it was doing, and somebody comparing two options on the setup page
	// would see them ordered by luck.
	MaxEntries int64
}

// DefaultMaxEntries is how many directory entries a walk will look at before
// calling the answer partial.
//
// Two million is a large home directory and several minutes of a slow disk. A
// walk that reaches it has found somebody with a node_modules problem, and the
// honest thing to show them is "more than 180 GB" rather than a number
// arrived at twenty minutes later when they have stopped looking.
const DefaultMaxEntries = 2_000_000

// Walk measures what the roots would contribute to a backup.
//
// onProgress, if given, is called every so often with the result so far, so a
// page that refreshes itself can show a figure climbing rather than a spinner.
// It is called from the walking goroutine: an implementation that blocks slows
// the walk, so the caller stores the value and returns.
//
// It never returns an error. Every failure it can meet — a directory it may
// not read, a file that vanished mid-walk, a root that does not exist — is a
// fact about the estimate rather than a reason to refuse to give one, and each
// is counted in the result instead.
func Walk(ctx context.Context, roots []string, opts Options, onProgress func(Result)) Result {
	started := time.Now()

	max := opts.MaxEntries
	if max <= 0 {
		max = DefaultMaxEntries
	}

	w := walker{
		matcher:  Excludes(opts.Excludes),
		larger:   opts.LargerThan,
		max:      max,
		progress: onProgress,
	}

	for _, root := range roots {
		if w.result.Partial {
			break
		}

		w.walk(ctx, root)
	}

	w.result.Took = time.Since(started)

	return w.result
}

// progressEvery is how many entries pass between calls to onProgress.
//
// Often enough that the number on a refreshing page moves, rarely enough that
// the callback is not the expensive part of the walk.
const progressEvery = 20_000

type walker struct {
	matcher  Matcher
	larger   int64
	max      int64
	progress func(Result)

	seen   int64
	result Result
}

func (w *walker) walk(ctx context.Context, root string) {
	// Not WalkDir's own error argument: a root that does not exist is the
	// ordinary case here (no ~/Videos on a work machine), and it must not
	// produce an "unreadable" count that makes the estimate look untrustworthy.
	if _, err := os.Lstat(root); err != nil {
		return
	}

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			w.result.Partial = true

			return filepath.SkipAll
		}

		w.seen++

		if w.seen > w.max {
			w.result.Partial = true

			return filepath.SkipAll
		}

		if w.seen%progressEvery == 0 && w.progress != nil {
			w.progress(w.result)
		}

		if err != nil {
			// The error arrives on the directory that could not be read, so
			// this counts places rather than files — which is the number worth
			// showing anyway. A file that vanished between the listing and the
			// stat lands here too and is indistinguishable, which is fine: both
			// mean "this much was not looked at".
			w.result.Unreadable++

			return nil
		}

		if path != root && w.matcher.Match(path, d.IsDir()) {
			w.result.Excluded++

			if d.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if d.IsDir() || !d.Type().IsRegular() {
			// Symlinks are deliberately not followed and deliberately not
			// counted. restic stores the link itself, which is a few bytes,
			// and following them is how a walk of a home directory ends up
			// counting the whole filesystem twice.
			return nil
		}

		info, err := d.Info()
		if err != nil {
			w.result.Unreadable++

			return nil
		}

		if w.larger > 0 && info.Size() >= w.larger {
			w.result.Excluded++

			return nil
		}

		w.result.Files++
		w.result.Bytes += info.Size()

		return nil
	})
}

// Matcher decides whether a path is excluded.
type Matcher []string

// Excludes builds a matcher from restic-style patterns.
//
// # This is an approximation, and it is the right one
//
// restic's exclude language is richer than what is implemented here: it has
// `**`, case-insensitive variants, `--iexclude`, exclusion files with their own
// rules, and several behaviours that depend on whether a pattern is anchored.
// Reimplementing it would produce a second, subtly different implementation of
// something whose whole purpose is to agree with the first — and the place the
// disagreement would show up is a backup that left out somebody's files while
// a page said it had not.
//
// So the authority on what is excluded is restic, always, at backup time. This
// is a sizing estimate, and it handles the three shapes that actually occur in
// the lists this program writes and that people type:
//
//	/home/jeff/.cache     an absolute path, and everything under it
//	node_modules          a bare name, matched against every path component
//	*.iso                 a glob, matched against the file name
//
// A pattern it gets wrong makes a number on the setup page slightly off. It
// cannot make a backup wrong, because nothing here is ever given to restic.
func Excludes(patterns []string) Matcher {
	out := make(Matcher, 0, len(patterns))

	for _, p := range patterns {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.Clean(p))
		}
	}

	return out
}

// Match reports whether path is excluded.
func (m Matcher) Match(path string, isDir bool) bool {
	if len(m) == 0 {
		return false
	}

	base := filepath.Base(path)

	for _, p := range m {
		switch {
		case filepath.IsAbs(p):
			if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
				return true
			}

		case strings.ContainsRune(p, filepath.Separator):
			// A relative pattern with a separator in it — "Library/Caches" —
			// matches anywhere it appears as a suffix of the path, which is
			// how somebody means it when they write it.
			if ok, _ := filepath.Match(p, path); ok {
				return true
			}

			if strings.HasSuffix(path, string(filepath.Separator)+p) {
				return true
			}

		default:
			if ok, _ := filepath.Match(p, base); ok {
				return true
			}
		}
	}

	return false
}
