package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
)

// RestoreOpts controls a restore.
type RestoreOpts struct {
	ThreeWay bool
	Force    bool     // confirm restoring an overlapped entry
	Paths    []string // restrict restore to these pathspecs (nil = all)
}

// RestorePlan is the read-only preview of what a restore would do:
// the three-class split (checkout vs delete) and the gates that --force/--3way
// override, computed without touching the work-tree or the journal.
type RestorePlan struct {
	Mode     string   // "three-class" or "3way"
	Checkout []string // M/D paths restored from the pre snapshot
	Delete   []string // A (command-created) paths removed
	Gates    []string // gates active under --force/--3way (overlapped/pre-only/target-changed)
	PreOnly  bool
}

// restoreGates validates the gates shared by dry-run and real restore and returns
// the ones that --force/--3way override (for preview visibility). It is
// read-only: no snapshot, no work-tree change. target-changed is evaluated
// against the work-tree directly, so the result is identical before or after the
// pre-restore snapshot (which never edits work-tree content).
func (e *Engine) restoreGates(ctx context.Context, repo *gitx.Repo, entry journal.Entry, opts RestoreOpts) (gates []string, preOnly bool, err error) {
	if entry.Overlapped {
		if !opts.Force {
			return nil, false, ErrOverlapped
		}
		gates = append(gates, "overlapped")
	}
	if entry.PreSHA == "" {
		return nil, false, fmt.Errorf("entry has no pre snapshot to restore from")
	}
	preOnly = entry.PostSHA == ""
	if preOnly {
		if !opts.Force {
			return nil, false, ErrPreOnly
		}
		gates = append(gates, "pre-only")
	}
	if !repo.CommitExists(ctx, entry.PreSHA) {
		return nil, false, reclaimedErr(entry.PreSHA)
	}
	if !preOnly && !repo.CommitExists(ctx, entry.PostSHA) {
		return nil, false, reclaimedErr(entry.PostSHA)
	}
	if !preOnly {
		changed, cerr := repo.WorktreeChangedSince(ctx, entry.PostSHA, opts.Paths)
		if cerr != nil {
			return nil, false, cerr
		}
		if changed {
			if !opts.ThreeWay {
				return nil, false, ErrTargetChanged
			}
			gates = append(gates, "target-changed")
		}
	}
	return gates, preOnly, nil
}

// RestorePlan computes the dry-run preview. It honors the gates identically to a
// real restore (an unforced gate returns the same error), so --dry-run is a
// faithful preview, not a gate bypass.
func (e *Engine) RestorePlan(ctx context.Context, workdir string, entry journal.Entry, opts RestoreOpts) (RestorePlan, error) {
	repo := e.RepoFor(workdir, entry.SessionID)
	if !repo.Initialized() {
		return RestorePlan{}, reclaimedErr(entry.PreSHA)
	}
	if _, err := e.EnsureRepo(ctx, workdir, entry.SessionID); err != nil {
		return RestorePlan{}, err
	}
	gates, preOnly, err := e.restoreGates(ctx, repo, entry, opts)
	if err != nil {
		return RestorePlan{}, err
	}
	var diff []gitx.DiffEntry
	if preOnly {
		diff, err = repo.DiffNameStatusWorktree(ctx, entry.PreSHA, opts.Paths)
	} else {
		diff, err = repo.DiffNameStatus(ctx, entry.PreSHA, entry.PostSHA, opts.Paths)
	}
	if err != nil {
		return RestorePlan{}, err
	}
	if len(diff) == 0 {
		return RestorePlan{}, ErrNothingToRestore
	}
	plan := RestorePlan{Mode: "three-class", Gates: gates, PreOnly: preOnly}
	if opts.ThreeWay {
		plan.Mode = "3way"
	}
	for _, d := range diff {
		if d.Status == "A" {
			plan.Delete = append(plan.Delete, d.Path)
		} else {
			plan.Checkout = append(plan.Checkout, d.Path)
		}
	}
	return plan, nil
}

// ErrOverlapped is returned when restoring an overlapped entry without --force.
var ErrOverlapped = errors.New("entry is overlapped; review with `bashback diff`, narrow with `restore <key> <path>`, or pass --force (which may also undo changes made by concurrent commands)")

// ErrPreOnly is returned when restoring a pre-only (interrupted) entry without
// --force. There is no post snapshot, so the undo is computed against the
// current work-tree, which may contain unrelated later changes.
var ErrPreOnly = errors.New("entry is pre-only (interrupted; no post snapshot); pass --force to undo against the current work-tree")

// ErrTargetChanged is returned when the target paths were modified after the
// snapshot and --3way was not requested.
var ErrTargetChanged = errors.New("target changed since snapshot; pass --3way to merge")

// ErrNothingToRestore is returned when the path filter selects no changes.
var ErrNothingToRestore = errors.New("no changes to restore for the given paths")

// Restore undoes a journal entry's file side effects and returns a new
// status=restored entry (itself undoable) for the caller to append.
func (e *Engine) Restore(ctx context.Context, workdir string, entry journal.Entry, opts RestoreOpts) (journal.Entry, error) {
	repo := e.RepoFor(workdir, entry.SessionID)
	if !repo.Initialized() {
		return journal.Entry{}, reclaimedErr(entry.PreSHA)
	}
	if _, err := e.EnsureRepo(ctx, workdir, entry.SessionID); err != nil {
		return journal.Entry{}, err
	}
	// All gates (overlapped, pre-only, reclaimed, target-changed) are validated
	// before any work-tree change, so a refused restore is a no-op. preOnly: an
	// interrupted command left a pre snapshot but no post; the undo target becomes
	// the current work-tree, lazily snapshotted below.
	_, preOnly, err := e.restoreGates(ctx, repo, entry, opts)
	if err != nil {
		return journal.Entry{}, err
	}

	force := e.forceInclude(workdir)

	// Hard prerequisite: snapshot the current work-tree first (full tree, never
	// filtered). Aligns the index for --3way, makes the restore undoable, and
	// lets us detect post-snapshot user edits. For a pre-only
	// entry this same snapshot doubles as the lazily-captured post.
	preRestoreSHA, _, _, err := e.snap(ctx, repo, "bashback: pre-restore", force)
	if err != nil {
		return journal.Entry{}, err
	}

	postSHA := entry.PostSHA
	if preOnly {
		postSHA = preRestoreSHA
	}

	diff, err := repo.DiffNameStatus(ctx, entry.PreSHA, postSHA, opts.Paths)
	if err != nil {
		return journal.Entry{}, err
	}
	if len(diff) == 0 {
		return journal.Entry{}, ErrNothingToRestore
	}

	var note string
	if opts.ThreeWay {
		note, err = e.restoreThreeWay(ctx, repo, entry.PreSHA, postSHA, opts.Paths)
	} else {
		err = e.restoreThreeClass(ctx, repo, entry.PreSHA, diff)
	}
	if err != nil {
		return journal.Entry{}, err
	}

	// Record the restored state so the restore is itself undoable.
	newSHA, _, snapNote, serr := e.snap(ctx, repo, "bashback: post-restore", force)
	if serr != nil {
		return degradedRestoreEntry(e, entry, preRestoreSHA, serr), restoreSnapFailed(serr, preRestoreSHA)
	}
	notes := nonEmpty(note, snapNote)
	if preOnly {
		notes = append(notes, "lazy post snapshot (pre-only entry)")
	}
	files, omitted := summarizeFiles(diff)
	restored := journal.Entry{
		ToolUseID:    restoreID(entry, preRestoreSHA),
		SessionID:    entry.SessionID,
		TS:           e.now().UTC().Format(time.RFC3339),
		Command:      "restore of " + journal.DefaultKeyer.Key(entry),
		PreSHA:       preRestoreSHA,
		PostSHA:      newSHA,
		Status:       journal.StatusRestored,
		Note:         strings.TrimSpace(strings.Join(notes, "; ")),
		DurationMS:   0,
		Files:        files,
		FilesOmitted: omitted,
	}
	return restored, nil
}

// PartialSelection is the user's interactive choice for a hunk-level restore:
// a subset patch to reverse-apply (selected text hunks), plus
// whole-file lists — Checkout reverts M/D paths from the pre snapshot, Delete
// removes A (command-created) paths. The three are disjoint by construction: a
// file is reverted either through its hunks in Patch or through Checkout/Delete,
// never both.
type PartialSelection struct {
	Patch    []byte
	Checkout []string
	Delete   []string
}

// RestorePartial undoes a selected subset of a command's changes: it reverse-
// applies the chosen text hunks and reverts/removes the chosen whole files. It
// runs the same gates as a full restore *before* any work-tree change (a refused
// restore is a no-op), takes a pre-restore snapshot so the partial restore is
// itself undoable, and records a status=restored entry whose Files reflect what
// actually changed. Pre-only entries are unsupported: there is no post patch to
// slice.
func (e *Engine) RestorePartial(ctx context.Context, workdir string, entry journal.Entry, sel PartialSelection, opts RestoreOpts) (journal.Entry, error) {
	repo := e.RepoFor(workdir, entry.SessionID)
	if !repo.Initialized() {
		return journal.Entry{}, reclaimedErr(entry.PreSHA)
	}
	if _, err := e.EnsureRepo(ctx, workdir, entry.SessionID); err != nil {
		return journal.Entry{}, err
	}
	_, preOnly, err := e.restoreGates(ctx, repo, entry, opts)
	if err != nil {
		return journal.Entry{}, err
	}
	if preOnly {
		return journal.Entry{}, ErrPreOnly
	}
	if len(sel.Patch) == 0 && len(sel.Checkout) == 0 && len(sel.Delete) == 0 {
		return journal.Entry{}, ErrNothingToRestore
	}

	force := e.forceInclude(workdir)

	// Snapshot the current work-tree first (full tree), so the partial restore is
	// undoable and a refused apply leaves a recoverable baseline.
	preRestoreSHA, _, _, err := e.snap(ctx, repo, "bashback: pre-restore (partial)", force)
	if err != nil {
		return journal.Entry{}, err
	}

	if len(sel.Patch) > 0 {
		// Reverse-apply: the subset patch is pre..post, so applying it in reverse
		// undoes exactly the selected hunks. Apply (not Apply3Way) fails outright on
		// a non-applying subset rather than leaving conflict markers.
		if err := repo.Apply(ctx, sel.Patch, true); err != nil {
			return journal.Entry{}, err
		}
	}
	if len(sel.Checkout) > 0 {
		if err := repo.CheckoutPaths(ctx, entry.PreSHA, sel.Checkout); err != nil {
			return journal.Entry{}, err
		}
	}
	for _, rel := range sel.Delete {
		abs := filepath.Join(repo.WorkTree, rel)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return journal.Entry{}, err
		}
		rmdirEmptyParents(repo.WorkTree, filepath.Dir(abs))
	}

	// Record the restored state so the partial restore is itself undoable. Files
	// are the real delta of this restore (pre-restore..post-restore), so they name
	// exactly what the selection touched.
	newSHA, _, snapNote, serr := e.snap(ctx, repo, "bashback: post-restore (partial)", force)
	if serr != nil {
		return degradedRestoreEntry(e, entry, preRestoreSHA, serr), restoreSnapFailed(serr, preRestoreSHA)
	}
	diff, err := repo.DiffNameStatus(ctx, preRestoreSHA, newSHA, nil)
	if err != nil {
		return journal.Entry{}, err
	}
	files, omitted := summarizeFiles(diff)
	notes := append([]string{"partial (interactive)"}, nonEmpty(snapNote)...)
	restored := journal.Entry{
		ToolUseID:    restoreID(entry, preRestoreSHA),
		SessionID:    entry.SessionID,
		TS:           e.now().UTC().Format(time.RFC3339),
		Command:      "restore of " + journal.DefaultKeyer.Key(entry),
		PreSHA:       preRestoreSHA,
		PostSHA:      newSHA,
		Status:       journal.StatusRestored,
		Note:         strings.TrimSpace(strings.Join(notes, "; ")),
		Files:        files,
		FilesOmitted: omitted,
	}
	return restored, nil
}

// restoreThreeClass: checkout modified/deleted from pre; delete command-created
// files; rmdir emptied dirs. The diff is pre..post (post may be the
// lazily-snapshotted work-tree for a pre-only entry).
func (e *Engine) restoreThreeClass(ctx context.Context, repo *gitx.Repo, preSHA string, diff []gitx.DiffEntry) error {
	var restore, added []string
	for _, d := range diff {
		if d.Status == "A" {
			added = append(added, d.Path)
		} else {
			restore = append(restore, d.Path)
		}
	}
	if len(restore) > 0 {
		if err := repo.CheckoutPaths(ctx, preSHA, restore); err != nil {
			return err
		}
	}
	for _, rel := range added {
		abs := filepath.Join(repo.WorkTree, rel)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		rmdirEmptyParents(repo.WorkTree, filepath.Dir(abs))
	}
	return nil
}

// restoreThreeWay reverse-applies the pre..post patch, preserving the user's
// non-overlapping edits and surfacing conflicts as standard markers rather than
// overwriting silently.
func (e *Engine) restoreThreeWay(ctx context.Context, repo *gitx.Repo, from, to string, paths []string) (string, error) {
	patch, err := repo.DiffPatch(ctx, from, to, paths)
	if err != nil {
		return "", err
	}
	if err := repo.Apply3Way(ctx, patch, true); err != nil {
		var ee *gitx.ExitError
		if errors.As(err, &ee) && ee.Code == 1 && strings.Contains(strings.ToLower(ee.Stderr), "conflict") {
			return "restore left conflict markers (3way)", nil
		}
		return "", err
	}
	return "", nil
}

func (e *Engine) forceInclude(workdir string) []string {
	m, err := e.Layout.ReadMeta(workdir)
	if err != nil {
		return nil
	}
	return m.ForceInclude
}

func restoreID(entry journal.Entry, preRestoreSHA string) string {
	base := journal.DefaultKeyer.Key(entry)
	if base == "" {
		base = entry.PostSHA
	}
	short := preRestoreSHA
	if len(short) > 8 {
		short = short[:8]
	}
	return "restore_" + short + "_" + base
}

// rmdirEmptyParents removes now-empty directories walking up to (but not past)
// the work-tree root.
func rmdirEmptyParents(root, dir string) {
	root = filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return // non-empty or gone
		}
		dir = filepath.Dir(dir)
	}
}

// degradedRestoreEntry is the journal row handed back when a restore mutated the
// work-tree but its post-restore snapshot failed. Losing the record would break
// "every restore is itself undoable", so the caller still journals this: its
// PreSHA points at the recoverable pre-restore state.
func degradedRestoreEntry(e *Engine, entry journal.Entry, preRestoreSHA string, serr error) journal.Entry {
	return journal.Entry{
		ToolUseID: restoreID(entry, preRestoreSHA),
		SessionID: entry.SessionID,
		TS:        e.now().UTC().Format(time.RFC3339),
		Command:   "restore of " + journal.DefaultKeyer.Key(entry),
		PreSHA:    preRestoreSHA,
		Status:    journal.StatusRestored,
		Note:      "post-restore snapshot failed: " + serr.Error() + "; pre-restore state saved as " + preRestoreSHA,
	}
}

func restoreSnapFailed(serr error, preRestoreSHA string) error {
	return fmt.Errorf("restore applied but the post-restore snapshot failed: %w (pre-restore state saved as %s)", serr, preRestoreSHA)
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
