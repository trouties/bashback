package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// Undo reverts the most recent command that actually changed files.
// It is sugar over restore: it never forces, so a gated candidate is refused with
// a pointer to the explicit `restore <key> --force`.
func Undo(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	threeWay := fs.Bool("3way", false, "preserve later user edits via three-way merge")
	dryRun := fs.Bool("dry-run", false, "preview without changing anything")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	session := fs.String("session", "", "scope undo to this session id prefix")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback undo [--3way] [--dry-run] [--json] [--session <id>]")
		fs.PrintDefaults()
	}
	if code, done := parseFS(fs, args, stdout, stderr); done {
		return code
	}

	entries, err := readView(layout, workdir)
	if err != nil {
		return errf(stderr, "read journal: %v", err)
	}

	// Multi-session surprise gate: explicit --session scopes candidates
	// to one session (and bypasses the gate — naming a session is consent). The
	// bare `undo` refuses when two or more sessions are concurrently active, since
	// the global-newest candidate may belong to a session the user is not watching.
	// Both checks run before candidate selection and dry-run evaluation.
	if *session != "" {
		scoped, gerr := scopeToSession(entries, *session, stderr)
		if gerr != 0 {
			return gerr
		}
		entries = scoped
	} else if act := activeSessions(entries, time.Now()); len(act) >= 2 {
		return undoSessionGate(stderr, act)
	}

	cand, ok := undoCandidate(entries)
	if !ok {
		return errf(stderr, "nothing to undo (no command with file changes to revert)")
	}
	key := journal.DefaultKeyer.Key(cand)

	opts := snapshot.RestoreOpts{ThreeWay: *threeWay}
	eng := newEngine(layout)

	if *dryRun {
		return restoreDryRun(eng, workdir, key, cand, opts, *jsonOut, stdout, stderr)
	}

	lock, code := acquireProjectLock(layout, workdir, stderr)
	if lock == nil {
		return code
	}
	defer func() { _ = lock.Release() }()

	restored, err := eng.Restore(ctx(), workdir, cand, opts)
	if err != nil {
		if restored.PreSHA != "" {
			_ = journal.Append(layout.JournalPath(workdir), restored)
		}
		return undoError(stderr, key, err)
	}
	if aerr := journal.Append(layout.JournalPath(workdir), restored); aerr != nil {
		fmt.Fprintf(stderr, "bashback: warning: undo succeeded but journaling it failed: %v\n", aerr)
	}
	if *jsonOut {
		return emitJSON(stdout, stderr, newRestoreResultJSON(key, restored))
	}
	fmt.Fprintf(stdout, "undid %q (new snapshot %s, reverse with: bashback restore %s)\n",
		key, truncate(restored.PostSHA, 12), journal.DefaultKeyer.Key(restored))
	if restored.Note != "" {
		fmt.Fprintf(stdout, "note: %s\n", restored.Note)
	}
	return 0
}

// undoCandidate returns the newest entry with real file changes to revert:
// post present, post != pre, and a protected or restored status. It skips
// skipped_no_change, unprotected_*, pre-only half-rows, and manual snapshots.
// A reclaimed candidate is still selected — undo stops and errors on it rather
// than silently skipping to an older entry.
func undoCandidate(entries []journal.Entry) (journal.Entry, bool) {
	for _, e := range tsDescOrder(entries) {
		if e.PostSHA == "" || e.PostSHA == e.PreSHA {
			continue
		}
		if e.Status == journal.StatusProtected || e.Status == journal.StatusRestored {
			return e, true
		}
	}
	return journal.Entry{}, false
}

// scopeToSession filters entries to those whose session id has the given prefix.
// It returns (filtered, 0) on a unique match, or (nil, 1) after reporting either
// no match or an ambiguous prefix that spans more than one session.
func scopeToSession(entries []journal.Entry, prefix string, stderr io.Writer) ([]journal.Entry, int) {
	matched := make([]journal.Entry, 0, len(entries))
	ids := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.SessionID, prefix) {
			matched = append(matched, e)
			ids[e.SessionID] = true
		}
	}
	switch {
	case len(ids) == 0:
		return nil, errf(stderr, "no session matches prefix %q", prefix)
	case len(ids) > 1:
		short := make([]string, 0, len(ids))
		for id := range ids {
			short = append(short, shortSession(id))
		}
		sort.Strings(short)
		return nil, errf(stderr, "session prefix %q is ambiguous: matches %s", prefix, strings.Join(short, ", "))
	default:
		return matched, 0
	}
}

// undoSessionGate refuses a bare undo while multiple sessions are active, listing
// each so the user can re-run with --session or address an entry explicitly.
func undoSessionGate(stderr io.Writer, act []sessionActivity) int {
	now := time.Now()
	fmt.Fprintln(stderr, "bashback: multiple sessions are active — refusing to guess which to undo:")
	for _, a := range act {
		fmt.Fprintf(stderr, "  session %s  last %s  %s\n", shortSession(a.ID), humanAge(a.LastTS, now), truncate(a.LastCommand, 50))
	}
	fmt.Fprintln(stderr, "pass --session <id> or use 'bashback restore <key>'")
	return 1
}

// undoError maps gate refusals to undo-specific guidance: undo never forces, so
// it points at the explicit `restore <key> --force/--3way`.
func undoError(stderr io.Writer, key string, err error) int {
	switch {
	case errors.Is(err, snapshot.ErrOverlapped):
		return errf(stderr, "%q is overlapped; a concurrent command's partial work may be entangled in its snapshot. Review with `bashback diff %s`, then narrow with `bashback restore %s <path>...`, or run `bashback restore %s --force` (may also undo concurrent commands' changes)", key, key, key, key)
	case errors.Is(err, snapshot.ErrTargetChanged):
		return errf(stderr, "%q changed since its snapshot; undo will not overwrite your later edits — run `bashback restore %s --3way`", key, key)
	case errors.Is(err, snapshot.ErrSnapshotReclaimed):
		return errf(stderr, "the most recent undoable command %q was reclaimed by gc; see its journal record (`bashback list`)", key)
	case errors.Is(err, snapshot.ErrNothingToRestore):
		return errf(stderr, "nothing to undo for %q", key)
	default:
		return errf(stderr, "undo: %v", err)
	}
}
