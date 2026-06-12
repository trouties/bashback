package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// Restore undoes a command's file side effects. Each refusal message states
// precisely why a restore is blocked (overlapped, reclaimed, pre-only, and so on).
func Restore(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	threeWay := fs.Bool("3way", false, "preserve later user edits via three-way merge")
	force := fs.Bool("force", false, "confirm restoring an overlapped or pre-only (interrupted) entry")
	dryRun := fs.Bool("dry-run", false, "preview the restore plan without changing anything")
	patch := fs.Bool("p", false, "interactively pick hunks to revert (requires a terminal)")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback restore <key> [--3way] [--force] [--dry-run] [-p] [--json] [path...]")
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
	restorePaths := rest[1:]

	// -p is a content-picking mode: it cannot combine with the whole-file --3way
	// merge, and it needs a terminal to prompt on (a pipe would block or apply
	// blindly). Both are flag-level refusals, before any journal/work-tree work.
	if *patch && *threeWay {
		return errf(stderr, "-p and --3way are mutually exclusive")
	}
	if *patch && !isInteractive() {
		return errf(stderr, "interactive restore (-p) requires an interactive terminal; use path filters or --3way")
	}

	e, ok, err := resolveEntry(layout, workdir, key)
	if err != nil {
		return errf(stderr, "%v", err)
	}
	if !ok {
		return errf(stderr, "no entry with key %q; see 'bashback list'", key)
	}
	if e.Status == journal.StatusManual {
		return errf(stderr, "%q is a manual checkpoint (pre-only by design); restore it with `bashback rewind %s`", key, key)
	}

	opts := snapshot.RestoreOpts{ThreeWay: *threeWay, Force: *force, Paths: restorePaths}
	eng := newEngine(layout)

	if *dryRun {
		return restoreDryRun(eng, workdir, key, e, opts, *jsonOut, stdout, stderr)
	}

	lock, code := acquireProjectLock(layout, workdir, stderr)
	if lock == nil {
		return code
	}
	defer func() { _ = lock.Release() }()

	if *patch {
		return restoreInteractive(eng, layout, workdir, key, e, opts, stdout, stderr)
	}

	restored, err := eng.Restore(ctx(), workdir, e, opts)
	if err != nil {
		// A degraded entry (post-restore snap failed) still carries a recoverable
		// pre-restore sha; record it so the half-applied restore stays undoable.
		if restored.PreSHA != "" {
			_ = journal.Append(layout.JournalPath(workdir), restored)
		}
		return restoreError(stderr, key, opts, err)
	}

	if aerr := journal.Append(layout.JournalPath(workdir), restored); aerr != nil {
		fmt.Fprintf(stderr, "bashback: warning: restore succeeded but journaling it failed: %v\n", aerr)
	}
	if *jsonOut {
		return emitJSON(stdout, stderr, newRestoreResultJSON(key, restored))
	}
	fmt.Fprintf(stdout, "restored %q (new snapshot %s, undo with: bashback restore %s)\n",
		key, truncate(restored.PostSHA, 12), journal.DefaultKeyer.Key(restored))
	if restored.Note != "" {
		fmt.Fprintf(stdout, "note: %s\n", restored.Note)
	}
	return 0
}

// restoreResultJSON is the machine-readable result of restore/rewind/undo: key
// is what was undone, undo_key undoes this undo, so an agent can self-correct.
type restoreResultJSON struct {
	V             int                  `json:"v"`
	Key           string               `json:"key"`
	UndoKey       string               `json:"undo_key"`
	PreRestoreSHA string               `json:"pre_restore_sha"`
	PostSHA       string               `json:"post_sha"`
	Note          string               `json:"note"`
	Files         []journal.FileChange `json:"files"`
}

// newRestoreResultJSON builds the payload from the original key and the new
// restored entry (whose pre_sha is the pre-restore snapshot, whose own key is
// the undo key).
func newRestoreResultJSON(origKey string, restored journal.Entry) restoreResultJSON {
	return restoreResultJSON{
		V:             outputVersion,
		Key:           origKey,
		UndoKey:       journal.DefaultKeyer.Key(restored),
		PreRestoreSHA: restored.PreSHA,
		PostSHA:       restored.PostSHA,
		Note:          restored.Note,
		Files:         restored.Files,
	}
}

// dryRunJSON is the --dry-run --json payload.
type dryRunJSON struct {
	V     int            `json:"v"`
	Key   string         `json:"key"`
	Mode  string         `json:"mode"`
	Gates []string       `json:"gates"`
	Plan  dryRunPlanJSON `json:"plan"`
}

type dryRunPlanJSON struct {
	Checkout []string `json:"checkout"`
	Delete   []string `json:"delete"`
}

func restoreDryRun(eng *snapshot.Engine, workdir, key string, e journal.Entry, opts snapshot.RestoreOpts, jsonOut bool, stdout, stderr io.Writer) int {
	plan, err := eng.RestorePlan(ctx(), workdir, e, opts)
	if err != nil {
		return restoreError(stderr, key, opts, err)
	}
	if jsonOut {
		out := dryRunJSON{
			V: outputVersion, Key: key, Mode: plan.Mode, Gates: plan.Gates,
			Plan: dryRunPlanJSON{Checkout: plan.Checkout, Delete: plan.Delete},
		}
		if out.Gates == nil {
			out.Gates = []string{}
		}
		if out.Plan.Checkout == nil {
			out.Plan.Checkout = []string{}
		}
		if out.Plan.Delete == nil {
			out.Plan.Delete = []string{}
		}
		return emitJSON(stdout, stderr, out)
	}
	fmt.Fprintf(stdout, "dry-run: restore %q (%s mode)\n", key, plan.Mode)
	if len(plan.Gates) > 0 {
		fmt.Fprintf(stdout, "  gates overridden by --force/--3way: %s\n", strings.Join(plan.Gates, ", "))
	}
	fmt.Fprintf(stdout, "  would checkout from pre snapshot (%d): %s\n", len(plan.Checkout), strings.Join(plan.Checkout, ", "))
	fmt.Fprintf(stdout, "  would delete command-created (%d): %s\n", len(plan.Delete), strings.Join(plan.Delete, ", "))
	fmt.Fprintln(stdout, "  (no changes made; re-run without --dry-run to apply)")
	return 0
}

// restoreInteractive drives the -p flow: gates fire here (before any prompt),
// then the pre..post patch is split and the user's per-hunk/per-file selection is
// applied via Engine.RestorePartial. Quitting or selecting nothing is a clean
// no-op with exit 0.
func restoreInteractive(eng *snapshot.Engine, layout paths.Layout, workdir, key string, e journal.Entry, opts snapshot.RestoreOpts, stdout, stderr io.Writer) int {
	plan, err := eng.RestorePlan(ctx(), workdir, e, opts)
	if err != nil {
		return restoreError(stderr, key, opts, err)
	}
	patch, err := eng.RepoFor(workdir, e.SessionID).DiffPatch(ctx(), e.PreSHA, e.PostSHA, opts.Paths)
	if err != nil {
		return errf(stderr, "restore -p: %v", err)
	}
	sel, aborted := runInteractive(parsePatch(string(patch)), plan, restoreInput, stdout)
	if aborted || (len(sel.Patch) == 0 && len(sel.Checkout) == 0 && len(sel.Delete) == 0) {
		fmt.Fprintln(stdout, "nothing selected; no changes made")
		return 0
	}

	restored, err := eng.RestorePartial(ctx(), workdir, e, sel, opts)
	if err != nil {
		if restored.PreSHA != "" {
			_ = journal.Append(layout.JournalPath(workdir), restored)
		}
		return restoreError(stderr, key, opts, err)
	}
	if aerr := journal.Append(layout.JournalPath(workdir), restored); aerr != nil {
		fmt.Fprintf(stderr, "bashback: warning: restore succeeded but journaling it failed: %v\n", aerr)
	}
	fmt.Fprintf(stdout, "partially restored %q (new snapshot %s, undo with: bashback restore %s)\n",
		key, truncate(restored.PostSHA, 12), journal.DefaultKeyer.Key(restored))
	if restored.Note != "" {
		fmt.Fprintf(stdout, "note: %s\n", restored.Note)
	}
	return 0
}

func restoreError(stderr io.Writer, key string, opts snapshot.RestoreOpts, err error) int {
	switch {
	case errors.Is(err, snapshot.ErrPreOnly):
		return errf(stderr, "%q is pre-only (interrupted; no post snapshot); re-run with --force to undo against the current work-tree", key)
	case errors.Is(err, snapshot.ErrOverlapped):
		return errf(stderr, "%q is overlapped; a concurrent command's partial work may be entangled in its snapshot. Review with `bashback diff %s`, then narrow the blast radius with `bashback restore %s <path>...`, or re-run with --force (--force may also undo changes made by concurrent commands)", key, key, key)
	case errors.Is(err, snapshot.ErrTargetChanged):
		// Once --force is in play the only remaining safe path is the full combo:
		// spell it out so the user does not have to discover --3way separately.
		if opts.Force {
			return errf(stderr, "target changed since the snapshot; re-run with --force --3way to merge your later edits (will not overwrite them)")
		}
		return errf(stderr, "target changed since the snapshot; re-run with --3way to merge (will not overwrite your edits)")
	case errors.Is(err, snapshot.ErrNothingToRestore):
		return errf(stderr, "nothing to restore for the given paths")
	case errors.Is(err, snapshot.ErrSnapshotReclaimed):
		return errf(stderr, "snapshot for %q was reclaimed by gc; see the journal record (`bashback list`)", key)
	default:
		return errf(stderr, "restore: %v", err)
	}
}
