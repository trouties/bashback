package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// bgCapture records a backgrounded command: a real pre/post pair (the post taken
// at the backgrounding moment) plus a bg_task_id, mirroring what the daemon writes
// for a `run_in_background` command.
func bgCapture(t *testing.T, h *harness, toolID, bgTaskID, tsRFC3339 string) journal.Entry {
	t.Helper()
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := h.e.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := h.e.Post(ctx(), repo, pre, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := journal.Entry{
		ToolUseID: toolID, SessionID: h.session, TS: tsRFC3339,
		Command: journal.RedactCommand("sleep 600 &"),
		PreSHA:  pre.PreSHA, PostSHA: post.PostSHA, Status: post.Status,
		BgTaskID: bgTaskID, Note: "background",
		Files: post.Files, FilesOmitted: post.FilesOmitted,
	}
	if err := journal.Append(h.e.Layout.JournalPath(h.work), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func bgMerged(t *testing.T, h *harness) map[string]journal.Entry {
	t.Helper()
	entries, err := journal.ReadMerged(h.e.Layout.JournalPath(h.work), journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]journal.Entry{}
	for _, e := range entries {
		m[e.ToolUseID] = e
	}
	return m
}

// When a backgrounded command writes after backgrounding, BgFinal records a
// synthetic bgfinal entry pairing the original post with a fresh snapshot, and
// points the original entry's note at it.
func TestBgFinalCreatesSyntheticEntry(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	orig := bgCapture(t, h, "tbg", "b32gd3xrm", "2026-06-10T00:00:01Z")

	h.write(t, "out.log", "finished-A\n")
	h.write(t, "result.txt", "done")

	res, err := h.e.BgFinal(ctx(), h.work, h.session, "b32gd3xrm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Key != "bgfinal_tbg" {
		t.Fatalf("BgFinal result = %+v, want Created bgfinal_tbg", res)
	}

	m := bgMerged(t, h)
	bg := m["bgfinal_tbg"]
	if bg.PreSHA != orig.PostSHA {
		t.Fatalf("bgfinal pre %q != original post %q", bg.PreSHA, orig.PostSHA)
	}
	if bg.PostSHA == "" || bg.PostSHA == orig.PostSHA {
		t.Fatalf("bgfinal post should be a fresh snapshot, got %q", bg.PostSHA)
	}
	if bg.Status != journal.StatusProtected {
		t.Fatalf("bgfinal status = %q, want protected", bg.Status)
	}
	if len(bg.Files) == 0 {
		t.Fatalf("bgfinal should record the background writes")
	}
	if !strings.Contains(bg.Command, "background completion of") {
		t.Fatalf("bgfinal command = %q", bg.Command)
	}
	if note := m["tbg"].Note; !strings.Contains(note, "final state captured") || !strings.Contains(note, "bgfinal_tbg") {
		t.Fatalf("original note not updated to point at bgfinal: %q", note)
	}
}

// No writes after backgrounding: BgFinal records completion on the original entry
// and creates no synthetic entry.
func TestBgFinalNoChangeNotesOnly(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "x")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")

	res, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Fatalf("no-change completion should not create an entry: %+v", res)
	}
	m := bgMerged(t, h)
	if _, ok := m["bgfinal_tbg"]; ok {
		t.Fatal("no bgfinal entry expected when nothing changed")
	}
	if !strings.Contains(m["tbg"].Note, "completed") {
		t.Fatalf("original note should record completion: %q", m["tbg"].Note)
	}
}

// Repeated BgFinal (the agent polling TaskOutput) records the capture exactly
// once (idempotency).
func TestBgFinalIdempotent(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")
	h.write(t, "out.log", "done\n")

	r1, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil || !r1.Created {
		t.Fatalf("first BgFinal should create: %+v err=%v", r1, err)
	}
	r2, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Created {
		t.Fatalf("second BgFinal must be a no-op: %+v", r2)
	}
	entries, _ := journal.ReadMerged(h.e.Layout.JournalPath(h.work), journal.DefaultKeyer)
	n := 0
	for _, e := range entries {
		if e.ToolUseID == "bgfinal_tbg" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one bgfinal entry, got %d", n)
	}
}

// When gc reclaimed the original post before the completion event fires, BgFinal
// can't pair a baseline: it leaves a "not captured" note on the original entry,
// matches (so polling stops), and creates no synthetic entry.
func TestBgFinalReclaimedLeavesNote(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")
	h.write(t, "out.log", "done\n") // writes exist, but the post is about to vanish

	// Simulate gc reclaiming the session's shadow repo (the original post SHA).
	if err := os.RemoveAll(h.e.Layout.SessionGitDir(h.work, h.session)); err != nil {
		t.Fatal(err)
	}

	res, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil {
		t.Fatalf("reclaimed post must not error from the CommitExists path: %v", err)
	}
	if !res.Matched || res.Created {
		t.Fatalf("reclaimed should match-but-not-create, got %+v", res)
	}
	m := bgMerged(t, h)
	if _, ok := m["bgfinal_tbg"]; ok {
		t.Fatal("no bgfinal entry expected when the post was reclaimed")
	}
	if note := m["tbg"].Note; !strings.Contains(note, "not captured (snapshot reclaimed)") {
		t.Fatalf("original note should record the reclaim: %q", note)
	}
}

// The reclaim note is idempotent: a second completion event (polling) appends
// nothing new.
func TestBgFinalReclaimedNoteIdempotent(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")
	h.write(t, "out.log", "done\n")
	if err := os.RemoveAll(h.e.Layout.SessionGitDir(h.work, h.session)); err != nil {
		t.Fatal(err)
	}

	if _, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.e.Layout.JournalPath(h.work))
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Fatalf("second reclaimed call must not create: %+v", res)
	}
	after, err := os.ReadFile(h.e.Layout.JournalPath(h.work))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("second reclaimed call mutated the journal:\nbefore=%s\nafter=%s", before, after)
	}
}

// Symmetry decision: unlike the CommitExists reclaim path, an
// EnsureRepo failure KEEPS returning an error rather than no-op'ing. The error
// bubbles to the daemon Logger / hook.log, so the failure is still traceable.
func TestBgFinalEnsureRepoReturnsError(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")
	h.write(t, "out.log", "done\n")

	// Break EnsureRepoDirs by replacing the sessions dir with a regular file, while
	// leaving the journal (RepoDir/journal.jsonl) readable so the merge succeeds and
	// the failure lands precisely on EnsureRepo.
	sessions := filepath.Join(h.e.Layout.RepoDir(h.work), "sessions")
	if err := os.RemoveAll(sessions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil); err == nil {
		t.Fatal("EnsureRepo failure should return an error, not a silent no-op")
	}
}

// An unknown bg task id no-ops (fail-open) — no entry, no error.
func TestBgFinalUnknownTaskNoop(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f", "x")
	bgCapture(t, h, "tbg", "task1", "2026-06-10T00:00:01Z")

	res, err := h.e.BgFinal(ctx(), h.work, h.session, "no-such-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched || res.Created {
		t.Fatalf("unknown task should no-op, got %+v", res)
	}
}

// A note that merely contains the word "completed" as a substring (e.g. an
// oversized-exclusion path) must not be mistaken for the bg-completed sentinel:
// BgFinal must still capture the background writes.
func TestBgFinalSentinelIsExactNotSubstring(t *testing.T) {
	h := newHarness(t)
	h.write(t, "out.log", "")
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := h.e.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := h.e.Post(ctx(), repo, pre, nil)
	if err != nil {
		t.Fatal(err)
	}
	orig := journal.Entry{
		ToolUseID: "tbg", SessionID: h.session, TS: "2026-06-10T00:00:01Z",
		Command: journal.RedactCommand("sleep 600 &"),
		PreSHA:  pre.PreSHA, PostSHA: post.PostSHA, Status: post.Status,
		BgTaskID: "task1", Note: "excluded oversized: build/completed.bin",
	}
	if err := journal.Append(h.e.Layout.JournalPath(h.work), orig); err != nil {
		t.Fatal(err)
	}

	h.write(t, "result.txt", "done")
	res, err := h.e.BgFinal(ctx(), h.work, h.session, "task1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatalf("BgFinal was fooled by a substring 'completed' note and skipped capture: %+v", res)
	}
}
