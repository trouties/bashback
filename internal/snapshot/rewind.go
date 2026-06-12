package snapshot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
)

// ManualSessionID namespaces manual `snap` checkpoints, which run outside any
// Claude Code session. It is exempted from the rewind cross-session
// gate so a snap→rewind round-trip in a single real session doesn't trip it.
const ManualSessionID = "manual"

// Rewind restores the whole work-tree to entry X's pre-command state, undoing X
// and everything after it. The pre-rewind snapshot is the post candidate and the
// recovery set is the full pre_sha(X)..preRewindSHA diff, so it needs no post
// snapshot on X and works for pre-only and manual entries alike. Gates are the
// caller's responsibility (they hold the journal view); the returned entry is
// itself undoable. spanNote describes the rollback span for the journal.
func (e *Engine) Rewind(ctx context.Context, workdir string, entry journal.Entry, spanNote string) (journal.Entry, error) {
	if entry.PreSHA == "" {
		return journal.Entry{}, fmt.Errorf("entry has no pre snapshot to rewind to")
	}
	repo := e.RepoFor(workdir, entry.SessionID)
	if !repo.Initialized() {
		return journal.Entry{}, reclaimedErr(entry.PreSHA)
	}
	if _, err := e.EnsureRepo(ctx, workdir, entry.SessionID); err != nil {
		return journal.Entry{}, err
	}
	if !repo.CommitExists(ctx, entry.PreSHA) {
		return journal.Entry{}, reclaimedErr(entry.PreSHA)
	}

	force := e.forceInclude(workdir)
	// Snapshot the current whole tree first: it is the post candidate and makes
	// the rewind itself undoable.
	preRewindSHA, _, _, err := e.snap(ctx, repo, "bashback: pre-rewind", force)
	if err != nil {
		return journal.Entry{}, err
	}

	diff, err := repo.DiffNameStatus(ctx, entry.PreSHA, preRewindSHA, nil)
	if err != nil {
		return journal.Entry{}, err
	}
	if len(diff) == 0 {
		return journal.Entry{}, ErrNothingToRestore
	}
	if err := e.restoreThreeClass(ctx, repo, entry.PreSHA, diff); err != nil {
		return journal.Entry{}, err
	}

	newSHA, _, snapNote, serr := e.snap(ctx, repo, "bashback: post-rewind", force)
	if serr != nil {
		return degradedRestoreEntry(e, entry, preRewindSHA, serr), restoreSnapFailed(serr, preRewindSHA)
	}

	files, omitted := summarizeFiles(diff)
	notes := nonEmpty(spanNote, snapNote)
	return journal.Entry{
		ToolUseID:    rewindID(entry, preRewindSHA),
		SessionID:    entry.SessionID,
		TS:           e.now().UTC().Format(time.RFC3339),
		Command:      "rewind to " + journal.DefaultKeyer.Key(entry),
		PreSHA:       preRewindSHA,
		PostSHA:      newSHA,
		Status:       journal.StatusRestored,
		Note:         strings.TrimSpace(strings.Join(notes, "; ")),
		Files:        files,
		FilesOmitted: omitted,
	}, nil
}

// RewindPlan previews the files a rewind would touch (pre_sha(X)..work-tree),
// read-only via the scratch-index work-tree diff — no snapshot, no journal.
// Returns ErrSnapshotReclaimed if the pre snapshot is gone.
func (e *Engine) RewindPlan(ctx context.Context, workdir string, entry journal.Entry) ([]gitx.DiffEntry, error) {
	if entry.PreSHA == "" {
		return nil, fmt.Errorf("entry has no pre snapshot to rewind to")
	}
	repo := e.RepoFor(workdir, entry.SessionID)
	if !repo.Initialized() {
		return nil, reclaimedErr(entry.PreSHA)
	}
	if _, err := e.EnsureRepo(ctx, workdir, entry.SessionID); err != nil {
		return nil, err
	}
	if !repo.CommitExists(ctx, entry.PreSHA) {
		return nil, reclaimedErr(entry.PreSHA)
	}
	return repo.DiffNameStatusWorktree(ctx, entry.PreSHA, nil)
}

func rewindID(entry journal.Entry, preRewindSHA string) string {
	base := journal.DefaultKeyer.Key(entry)
	if base == "" {
		base = entry.PreSHA
	}
	short := preRewindSHA
	if len(short) > 8 {
		short = short[:8]
	}
	return "rewind_" + short + "_" + base
}
