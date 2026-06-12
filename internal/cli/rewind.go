package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// Rewind restores the whole work-tree to a command's pre state, undoing it and
// everything after. Three gates (cross-session span, uncommitted
// work-tree edits, overlapped target) each refuse without --force; --force
// confirms, and the pre-rewind snapshot keeps the rewind itself undoable.
func Rewind(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	force := fs.Bool("force", false, "confirm rewinding past gates (cross-session, uncommitted, overlapped)")
	dryRun := fs.Bool("dry-run", false, "preview the rewind span without changing anything")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback rewind <key> [--force] [--dry-run] [--json]")
		fs.PrintDefaults()
	}
	rest, code, done := parseFlagsAnywhere(fs, args, stdout, stderr)
	if done {
		return code
	}
	fs.SetOutput(stderr)
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	key := rest[0]

	entries, err := readView(layout, workdir)
	if err != nil {
		return errf(stderr, "read journal: %v", err)
	}
	entry, ok, rerr := resolveEntryIn(entries, key)
	if rerr != nil {
		return errf(stderr, "%v", rerr)
	}
	if !ok {
		return errf(stderr, "no entry with key %q; see 'bashback list'", key)
	}

	eng := newEngine(layout)
	gates := rewindGates(eng, workdir, entries, entry)

	// Compute the span for the preview/print (files touched + entries undone).
	plan, perr := eng.RewindPlan(ctx(), workdir, entry)
	if perr != nil {
		return rewindError(stderr, key, perr)
	}
	spanEntries := rewindSpanEntries(entries, entry)
	spanCount := len(spanEntries)

	if len(gates) > 0 && !*force {
		for _, g := range gates {
			fmt.Fprintf(stderr, "bashback: %s\n", g.message)
		}
		fmt.Fprintf(stderr, "bashback: preview first with `bashback rewind %s --dry-run --force`, then re-run `bashback rewind %s --force` to proceed\n", key, key)
		return 1
	}

	if *dryRun {
		return rewindDryRun(key, gates, plan, spanEntries, atNIndex(entries), shortKeys(entries), *jsonOut, stdout, stderr)
	}

	lock, code := acquireProjectLock(layout, workdir, stderr)
	if lock == nil {
		return code
	}
	defer func() { _ = lock.Release() }()

	spanNote := fmt.Sprintf("rewind span: %d entr%s, %d file%s", spanCount, plural(spanCount, "y", "ies"), len(plan), plural(len(plan), "", "s"))
	rew, err := eng.Rewind(ctx(), workdir, entry, spanNote)
	if err != nil {
		if rew.PreSHA != "" {
			_ = journal.Append(layout.JournalPath(workdir), rew)
		}
		return rewindError(stderr, key, err)
	}
	if aerr := journal.Append(layout.JournalPath(workdir), rew); aerr != nil {
		fmt.Fprintf(stderr, "bashback: warning: rewind succeeded but journaling it failed: %v\n", aerr)
	}
	if *jsonOut {
		return emitJSON(stdout, stderr, newRestoreResultJSON(key, rew))
	}
	fmt.Fprintf(stdout, "rewound to %q (undid %d entr%s across %d file%s; new snapshot %s)\n",
		key, spanCount, plural(spanCount, "y", "ies"), len(plan), plural(len(plan), "", "s"), truncate(rew.PostSHA, 12))
	fmt.Fprintf(stdout, "undo this rewind with: bashback restore %s\n", journal.DefaultKeyer.Key(rew))
	if rew.Note != "" {
		fmt.Fprintf(stdout, "note: %s\n", rew.Note)
	}
	return 0
}

type rewindGate struct {
	name    string
	message string
}

// rewindGates evaluates the three rewind gates against the journal view and the
// work-tree. Each returned gate blocks unless --force is passed.
func rewindGates(eng *snapshot.Engine, workdir string, entries []journal.Entry, x journal.Entry) []rewindGate {
	var gates []rewindGate

	// Gate 3: the target's pre may have caught a concurrent command's half-work.
	if x.Overlapped {
		gates = append(gates, rewindGate{"overlapped",
			fmt.Sprintf("target %q is overlapped; its pre snapshot may include a concurrent command's partial work", journal.DefaultKeyer.Key(x))})
	}

	// Gate 1: cross-session span. Count distinct real (non-manual) sessions whose
	// commands fall in the rewind span; more than one means other sessions' work
	// would also be undone.
	if n := distinctRealSessionsInSpan(entries, x); n > 1 {
		gates = append(gates, rewindGate{"cross-session",
			fmt.Sprintf("rewind spans %d sessions; --force will also undo changes made by other sessions in this span", n)})
	}

	// Gate 2: work-tree edits not captured by any snapshot would be discarded.
	if rewindHasUncommittedEdits(eng, workdir, entries, x) {
		gates = append(gates, rewindGate{"uncommitted",
			"the work-tree has edits not captured by bashback; --force will also undo them (the pre-rewind snapshot keeps this reversible)"})
	}

	return gates
}

// spanStart is X's pre-command instant: its post timestamp less its duration.
func spanStart(x journal.Entry) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339, x.TS)
	if err != nil {
		return time.Time{}, false
	}
	return ts.Add(-time.Duration(x.DurationMS) * time.Millisecond), true
}

// rewindSpanEntries returns the entries undone by the rewind: X and everything at
// or after its pre instant (any session), which is what the whole-tree restore
// reverts. The count is len(result); a span that resolves to nothing still counts
// X itself so the preview never reads as a no-op.
func rewindSpanEntries(entries []journal.Entry, x journal.Entry) []journal.Entry {
	start, ok := spanStart(x)
	if !ok {
		return []journal.Entry{x}
	}
	var span []journal.Entry
	for _, e := range entries {
		et, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			continue
		}
		if !et.Before(start) {
			span = append(span, e)
		}
	}
	if len(span) == 0 {
		span = []journal.Entry{x}
	}
	return span
}

func distinctRealSessionsInSpan(entries []journal.Entry, x journal.Entry) int {
	start, ok := spanStart(x)
	if !ok {
		return 1
	}
	seen := map[string]bool{}
	for _, e := range entries {
		et, err := time.Parse(time.RFC3339, e.TS)
		if err != nil || et.Before(start) {
			continue
		}
		if e.SessionID == "" || e.SessionID == snapshot.ManualSessionID {
			continue
		}
		seen[e.SessionID] = true
	}
	return len(seen)
}

// rewindHasUncommittedEdits reports whether the work-tree differs from the most
// recent snapshot bashback took (latest post, preferring X's own session), i.e.
// hand edits that no journal entry records.
func rewindHasUncommittedEdits(eng *snapshot.Engine, workdir string, entries []journal.Entry, x journal.Entry) bool {
	baselineSHA, baselineSession := "", ""
	ordered := tsDescOrder(entries)
	for _, e := range ordered {
		if e.SessionID == x.SessionID && e.PostSHA != "" {
			baselineSHA, baselineSession = e.PostSHA, e.SessionID
			break
		}
	}
	if baselineSHA == "" {
		for _, e := range ordered {
			if e.PostSHA != "" {
				baselineSHA, baselineSession = e.PostSHA, e.SessionID
				break
			}
		}
	}
	if baselineSHA == "" {
		return false
	}
	repo := eng.RepoFor(workdir, baselineSession)
	if !repo.CommitExists(ctx(), baselineSHA) {
		return false
	}
	changed, err := repo.WorktreeChangedSince(ctx(), baselineSHA, nil)
	if err != nil {
		return false
	}
	return changed
}

type rewindDryRunJSON struct {
	V           int                  `json:"v"`
	Key         string               `json:"key"`
	Entries     int                  `json:"entries"`
	Files       []string             `json:"files"`
	FileChanges []journal.FileChange `json:"file_changes"`
	Span        []rewindSpanJSON     `json:"span"`
	Gates       []string             `json:"gates"`
}

// rewindSpanJSON is one entry the rewind would undo, in the dry-run preview.
type rewindSpanJSON struct {
	Key     string `json:"key"`
	TS      string `json:"ts"`
	Command string `json:"command"`
}

// rewindDryRunFileCap / rewindDryRunSpanCap bound the textual preview; the --json
// payload is never capped.
const (
	rewindDryRunFileCap = 20
	rewindDryRunSpanCap = 10
)

func rewindDryRun(key string, gates []rewindGate, plan []gitx.DiffEntry, spanEntries []journal.Entry, atN map[string]int, short map[string]string, jsonOut bool, stdout, stderr io.Writer) int {
	files := make([]string, 0, len(plan))
	fileChanges := make([]journal.FileChange, 0, len(plan))
	for _, d := range plan {
		files = append(files, d.Path)
		fileChanges = append(fileChanges, journal.FileChange{P: d.Path, S: d.Status})
	}
	gateNames := make([]string, 0, len(gates))
	for _, g := range gates {
		gateNames = append(gateNames, g.name)
	}
	ordered := tsDescOrder(spanEntries)
	spanCount := len(ordered)

	if jsonOut {
		span := make([]rewindSpanJSON, 0, len(ordered))
		for _, e := range ordered {
			span = append(span, rewindSpanJSON{
				Key: journal.DefaultKeyer.Key(e), TS: e.TS, Command: e.Command,
			})
		}
		return emitJSON(stdout, stderr, rewindDryRunJSON{
			V: outputVersion, Key: key, Entries: spanCount, Files: files,
			FileChanges: fileChanges, Span: span, Gates: gateNames,
		})
	}

	fmt.Fprintf(stdout, "dry-run: rewind to %q would undo %d entr%s across %d file%s\n",
		key, spanCount, plural(spanCount, "y", "ies"), len(files), plural(len(files), "", "s"))

	if len(fileChanges) > 0 {
		fmt.Fprintln(stdout, "  files:")
		for i, fc := range fileChanges {
			if i == rewindDryRunFileCap {
				fmt.Fprintf(stdout, "    (+%d more)\n", len(fileChanges)-rewindDryRunFileCap)
				break
			}
			fmt.Fprintf(stdout, "    %s  %s\n", fc.S, fc.P)
		}
	}

	if len(ordered) > 0 {
		fmt.Fprintln(stdout, "  entries:")
		for i, e := range ordered {
			if i == rewindDryRunSpanCap {
				fmt.Fprintf(stdout, "    (+%d more)\n", len(ordered)-rewindDryRunSpanCap)
				break
			}
			k := journal.DefaultKeyer.Key(e)
			fmt.Fprintf(stdout, "    @%d  %s  %s\n", atN[k], keyCol(short, k, false), truncate(e.Command, 50))
		}
	}

	if len(gateNames) > 0 {
		fmt.Fprintf(stdout, "  gates (need --force): %s\n", strings.Join(gateNames, ", "))
	}
	fmt.Fprintln(stdout, "  (no changes made; re-run without --dry-run to apply)")
	return 0
}

func rewindError(stderr io.Writer, key string, err error) int {
	switch {
	case errors.Is(err, snapshot.ErrSnapshotReclaimed):
		return errf(stderr, "snapshot for %q was reclaimed by gc; see the journal record (`bashback list`)", key)
	case errors.Is(err, snapshot.ErrNothingToRestore):
		return errf(stderr, "nothing to rewind (the work-tree already matches %q's pre state)", key)
	default:
		return errf(stderr, "rewind: %v", err)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
