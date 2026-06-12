package snapshot

import (
	"context"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// bgFinalPrefix names the synthetic entry that captures a backgrounded command's
// final on-disk state. It does not start with `@` and does not collide with the
// toolu_*/restore_*/rewind_*/snap_* prefixes.
const bgFinalPrefix = "bgfinal_"

// NoteBgFinalCaptured / NoteBgCompleted are the original entry's updated note
// half-rows after a completion is observed. Both keep the "background" substring
// so `list`'s (background) label and isBackground stay true; both make the entry
// idempotent against repeated TaskOutput polls.
const (
	noteBgFinalCaptured = "background (final state captured: "
	noteBgCompleted     = "background (completed; no further writes)"
	// noteBgNotCaptured records that gc reclaimed the original post before the
	// completion event fired, so no final state could be paired. It keeps the
	// "background" prefix (isBackground stays true) and is matched as an
	// idempotency sentinel like noteBgCompleted.
	noteBgNotCaptured = "background final state not captured (snapshot reclaimed)"
)

// BgFinalResult reports the outcome of a background-completion capture so the
// caller can build its Response and (optionally) inject agent context.
type BgFinalResult struct {
	Matched bool   // an original entry with this bg task id was found
	Created bool   // a bgfinal entry was appended (the work-tree changed)
	Key     string // the bgfinal entry key, when Created
	OrigKey string
	Files   int
	Deletes int
}

// BgFinal captures the final on-disk state of a completed background command and
// records it as a synthetic bgfinal entry paired with the original command's post.
// It is the shared core of the daemon OpBgFinal worker and the
// degraded direct-write client; callers serialize it (daemon queue / flock).
//
// It is idempotent: once a bgfinal entry exists or the original note records
// completion, repeated calls (the agent polling TaskOutput) no-op. An unknown
// bg task id no-ops with Matched=false (fail-open). It must run at the moment the
// completion event fires — after the task is GC'd its writes can no longer be
// attributed (S6 critical constraint), but the on-disk state is still captured.
func (e *Engine) BgFinal(ctx context.Context, workdir, sessionID, bgTaskID string, forceInclude []string) (BgFinalResult, error) {
	if bgTaskID == "" {
		return BgFinalResult{}, nil
	}
	jpath := e.Layout.JournalPath(workdir)
	entries, err := journal.ReadMerged(jpath, journal.DefaultKeyer)
	if err != nil {
		return BgFinalResult{}, err
	}

	orig, ok := findByBgTaskID(entries, bgTaskID)
	if !ok || orig.PostSHA == "" {
		return BgFinalResult{}, nil // unknown task or no post baseline: no-op
	}
	origKey := journal.DefaultKeyer.Key(orig)
	bgKey := bgFinalPrefix + origKey

	// Idempotency: a prior capture left a bgfinal entry, or a prior no-change run
	// left a "completed" note. Either way, don't record again.
	for _, en := range entries {
		if en.ToolUseID == bgKey {
			return BgFinalResult{Matched: true, OrigKey: origKey, Key: bgKey}, nil
		}
	}
	if strings.Contains(orig.Note, noteBgCompleted) || strings.Contains(orig.Note, noteBgNotCaptured) {
		return BgFinalResult{Matched: true, OrigKey: origKey}, nil
	}

	repo, err := e.EnsureRepo(ctx, workdir, orig.SessionID)
	if err != nil {
		// Unlike the CommitExists path below, EnsureRepo failure returns an error
		// rather than no-op'ing — intentional, not accidental: it may be transient
		// and worth retrying. The error still bubbles to the daemon Logger and
		// hook.log, so the failure stays traceable either way.
		return BgFinalResult{}, err
	}
	// The original post must still resolve; if gc reclaimed it we cannot pair a
	// pre, so leave an idempotent note on the original entry and no-op rather than
	// record a dangling entry. list/show surface the note; doctor needs no new UI.
	if !repo.CommitExists(ctx, orig.PostSHA) {
		_ = journal.Append(jpath, journal.Entry{ToolUseID: orig.ToolUseID, Note: noteBgNotCaptured})
		return BgFinalResult{Matched: true, OrigKey: origKey}, nil
	}

	// Snapshot the current work-tree against the original post: Files is exactly
	// the writes that landed after backgrounding (plus any overlap, which the
	// interval check below flags honestly).
	post, err := e.Post(ctx, repo, PreResult{PreSHA: orig.PostSHA}, forceInclude)
	if err != nil {
		return BgFinalResult{}, err
	}

	now := e.now()
	if post.PostSHA == orig.PostSHA || len(post.Files) == 0 {
		// No further writes: just record completion on the original entry so polls
		// stay idempotent, and report no new entry.
		_ = journal.Append(jpath, journal.Entry{ToolUseID: orig.ToolUseID, Note: noteBgCompleted})
		return BgFinalResult{Matched: true, OrigKey: origKey}, nil
	}

	durationMS := int64(0)
	if t, perr := time.Parse(time.RFC3339, orig.TS); perr == nil {
		durationMS = now.Sub(t).Milliseconds()
	}
	bgEntry := journal.Entry{
		ToolUseID:    bgKey,
		SessionID:    orig.SessionID,
		TS:           now.UTC().Format(time.RFC3339),
		Command:      "background completion of: " + orig.Command,
		PreSHA:       orig.PostSHA,
		PostSHA:      post.PostSHA,
		Status:       journal.StatusProtected,
		DurationMS:   durationMS,
		Files:        post.Files,
		FilesOmitted: post.FilesOmitted,
		Origin:       orig.Origin,
	}
	if err := journal.Append(jpath, bgEntry); err != nil {
		return BgFinalResult{}, err
	}
	// Point the original entry at its bgfinal capture (later note wins on merge).
	_ = journal.Append(jpath, journal.Entry{ToolUseID: orig.ToolUseID, Note: noteBgFinalCaptured + bgKey + ")"})

	deletes := 0
	for _, f := range post.Files {
		if f.S == "D" {
			deletes++
		}
	}
	return BgFinalResult{
		Matched: true, Created: true, Key: bgKey, OrigKey: origKey,
		Files: len(post.Files), Deletes: deletes,
	}, nil
}

// findByBgTaskID returns the merged entry whose recorded background task id
// matches, if any.
func findByBgTaskID(entries []journal.Entry, bgTaskID string) (journal.Entry, bool) {
	for _, e := range entries {
		if e.BgTaskID == bgTaskID {
			return e, true
		}
	}
	return journal.Entry{}, false
}
