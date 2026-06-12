package snapshot

import (
	"context"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// Snap takes a manual whole-tree checkpoint in the shared per-project
// manual.git, outside any session. It is pre-only by design: the
// checkpoint SHA is the pre side, and the recovery verb is `rewind <snap-key>`.
// On an unchanged work-tree it reuses the last snapshot SHA (fast-path) and notes
// it. The caller serializes concurrent snaps (flock) and appends the entry.
func (e *Engine) Snap(ctx context.Context, workdir, message string) (journal.Entry, error) {
	repo, err := e.EnsureRepo(ctx, workdir, ManualSessionID)
	if err != nil {
		return journal.Entry{}, err
	}
	pre, err := e.Pre(ctx, repo, e.forceInclude(workdir))
	if err != nil {
		return journal.Entry{}, err
	}
	note := pre.Note
	if pre.Skipped {
		note = joinNote(note, "no changes since last snapshot")
	}
	short := pre.PreSHA
	if len(short) > 8 {
		short = short[:8]
	}
	return journal.Entry{
		ToolUseID: "snap_" + short,
		SessionID: ManualSessionID,
		TS:        e.now().UTC().Format(time.RFC3339),
		Command:   journal.RedactCommand(message),
		PreSHA:    pre.PreSHA,
		Status:    journal.StatusManual,
		Note:      note,
	}, nil
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}
