package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/snapshot"
)

func TestUndoOverlappedGuidanceMatchesRestoreLadder(t *testing.T) {
	var b bytes.Buffer
	undoError(&b, "k1", snapshot.ErrOverlapped)
	s := b.String()
	for _, want := range []string{"diff", "<path>", "--force"} {
		if !strings.Contains(s, want) {
			t.Errorf("overlapped guidance missing %q: %s", want, s)
		}
	}
	if !(strings.Index(s, "diff") < strings.Index(s, "<path>") && strings.Index(s, "<path>") < strings.Index(s, "--force")) {
		t.Errorf("guidance not in review->narrow->force order: %s", s)
	}
}

func TestUndoTargetChangedHasNoForceFalseLead(t *testing.T) {
	var b bytes.Buffer
	undoError(&b, "k1", snapshot.ErrTargetChanged)
	s := b.String()
	if !strings.Contains(s, "--3way") {
		t.Errorf("target-changed guidance should point at --3way: %s", s)
	}
	if strings.Contains(s, "(or `--force`)") {
		t.Errorf("target-changed guidance must not offer --force as a casual alternative: %s", s)
	}
}

// retimeJournal rewrites the recorded ts of the named entries (by tool_use_id) so
// tests can place captures inside or outside the window without wall-clock. Order is preserved.
func retimeJournal(t *testing.T, f *fix, byID map[string]string) {
	t.Helper()
	jp := f.layout.JournalPath(f.work)
	entries, err := journal.ReadMerged(jp, journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jp); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if ts, ok := byID[e.ToolUseID]; ok {
			e.TS = ts
		}
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}
}

// Two sessions with recent undoable entries make no-arg undo refuse, list both, and not touch the work-tree.
func TestUndoMultiSessionGate(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_s1", "echo s1", func() { f.write(t, "f.txt", "v1") })
	f.session = "sess2"
	f.write(t, "g.txt", "g0")
	f.capture(t, "tool_s2", "echo s2", func() { f.write(t, "g.txt", "g1") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1": tsAgo(now, 5*time.Minute),
		"tool_s2": tsAgo(now, 3*time.Minute),
	})

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 1 {
		t.Fatalf("multi-session undo should gate (exit 1), got %d: %s", code, errb.String())
	}
	got := errb.String()
	for _, want := range []string{shortSession("sess1"), shortSession("sess2"), "--session", "restore"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("gate output missing %q: %q", want, got)
		}
	}
	if readFile(t, f, "f.txt") != "v1" || readFile(t, f, "g.txt") != "g1" {
		t.Fatal("gate must not change the work-tree")
	}
}

// A second session with only stale (>window) entries does not activate the gate.
func TestUndoSingleSessionUnchanged(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_s1", "echo s1", func() { f.write(t, "f.txt", "v1") })
	f.session = "sess2"
	f.capture(t, "tool_s2old", "echo old", func() { f.write(t, "g.txt", "g1") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1":    tsAgo(now, 5*time.Minute),
		"tool_s2old": tsAgo(now, 2*time.Hour),
	})
	f.session = "sess1"

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("single active session should not gate, exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "f.txt") != "v0" {
		t.Fatalf("undo should revert the only recent change: %q", readFile(t, f, "f.txt"))
	}
}

// --session scopes the candidate to that session's newest undoable entry, bypassing the gate.
func TestUndoSessionFlag(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_s1", "echo s1", func() { f.write(t, "f.txt", "v1") })
	f.session = "sess2"
	f.write(t, "g.txt", "g0")
	f.capture(t, "tool_s2", "echo s2", func() { f.write(t, "g.txt", "g1") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1": tsAgo(now, 5*time.Minute),
		"tool_s2": tsAgo(now, 3*time.Minute),
	})

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--session", "sess2"}, &out, &errb); code != 0 {
		t.Fatalf("undo --session exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "g.txt") != "g0" {
		t.Fatalf("sess2 change should revert: %q", readFile(t, f, "g.txt"))
	}
	if readFile(t, f, "f.txt") != "v1" {
		t.Fatalf("sess1 must be untouched: %q", readFile(t, f, "f.txt"))
	}
}

// With two active sessions, --session is enough to proceed (the gate is only for the no-arg form).
func TestUndoSessionFlagBypassesGate(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_s1", "echo s1", func() { f.write(t, "f.txt", "v1") })
	f.session = "sess2"
	f.capture(t, "tool_s2", "echo s2", func() { f.write(t, "g.txt", "g1") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1": tsAgo(now, 5*time.Minute),
		"tool_s2": tsAgo(now, 3*time.Minute),
	})

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--session", "sess1"}, &out, &errb); code != 0 {
		t.Fatalf("--session should bypass the gate, exit %d: %s", code, errb.String())
	}
	if readFile(t, f, "f.txt") != "v0" {
		t.Fatalf("sess1 change should revert: %q", readFile(t, f, "f.txt"))
	}
}

// A prefix matching two distinct session ids is refused, lists the candidates, and changes nothing.
func TestUndoSessionPrefixAmbiguous(t *testing.T) {
	f := newFix(t)
	f.session = "abc111"
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_a", "echo a", func() { f.write(t, "f.txt", "v1") })
	f.session = "abc222"
	f.capture(t, "tool_b", "echo b", func() { f.write(t, "g.txt", "g1") })

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--session", "abc"}, &out, &errb); code != 1 {
		t.Fatalf("ambiguous prefix should exit 1, got %d: %s", code, errb.String())
	}
	got := errb.String()
	if !bytes.Contains([]byte(got), []byte("abc111")) || !bytes.Contains([]byte(got), []byte("abc222")) {
		t.Fatalf("ambiguous error should list candidates, got %q", got)
	}
	if readFile(t, f, "f.txt") != "v1" {
		t.Fatal("ambiguous undo must change nothing")
	}
}

// The gate fires before --dry-run is even evaluated.
func TestUndoDryRunGateToo(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_s1", "echo s1", func() { f.write(t, "f.txt", "v1") })
	f.session = "sess2"
	f.capture(t, "tool_s2", "echo s2", func() { f.write(t, "g.txt", "g1") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1": tsAgo(now, 5*time.Minute),
		"tool_s2": tsAgo(now, 3*time.Minute),
	})

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--dry-run"}, &out, &errb); code != 1 {
		t.Fatalf("dry-run must also gate, got %d: %s", code, errb.String())
	}
}

func readFile(t *testing.T, f *fix, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.work, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// undo reverts the latest change; a second undo toggles back (redo), since the
// restore entry is itself an undoable change.
func TestUndoToggle(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	f.capture(t, "tool_u", "edit", func() { f.write(t, "f.txt", "changed") })

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("undo exit %d: %s", code, errb.String())
	}
	if v := readFile(t, f, "f.txt"); v != "original" {
		t.Fatalf("after undo = %q, want original", v)
	}

	out.Reset()
	errb.Reset()
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("second undo exit %d: %s", code, errb.String())
	}
	if v := readFile(t, f, "f.txt"); v != "changed" {
		t.Fatalf("after second undo (redo) = %q, want changed", v)
	}
}

// undo skips skipped_no_change / pre-only / unprotected entries, landing on the newest real change.
func TestUndoSkipsNonCandidates(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	f.capture(t, "tool_real", "real", func() { f.write(t, "f.txt", "v2") })
	// A no-op command after it: skipped_no_change (post == pre).
	f.capture(t, "tool_noop", "noop", nil)
	// A pre-only interrupted command on top.
	f.preCapture(t, "tool_pre", "interrupted", func() { f.write(t, "g.txt", "half") })

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("undo exit %d: %s", code, errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("tool_real")) {
		t.Fatalf("undo should target the real change, got %q", out.String())
	}
	if v := readFile(t, f, "f.txt"); v != "v1" {
		t.Fatalf("undo did not revert the real change: %q", v)
	}
}

func TestUndoNothing(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code == 0 {
		t.Fatal("undo with nothing should fail")
	}
	if !bytes.Contains(errb.Bytes(), []byte("nothing to undo")) {
		t.Fatalf("want nothing-to-undo, got %q", errb.String())
	}
}

// A gated (overlapped) candidate is refused; undo points at restore --force, offering no --force of its own.
func TestUndoGatedRefusesAndGuides(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_g", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code == 0 {
		t.Fatal("overlapped candidate should refuse")
	}
	got := errb.String()
	if !bytes.Contains([]byte(got), []byte("restore")) || !bytes.Contains([]byte(got), []byte("--force")) {
		t.Fatalf("undo should guide to restore --force, got %q", got)
	}
	// File unchanged (no force).
	if v := readFile(t, f, "f.txt"); v != "b" {
		t.Fatalf("gated undo must not change the file: %q", v)
	}
}

func TestUndoDryRun(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	f.capture(t, "tool_ud", "x", func() { f.write(t, "f.txt", "changed") })

	jbefore, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("undo --dry-run exit %d: %s", code, errb.String())
	}
	if v := readFile(t, f, "f.txt"); v != "changed" {
		t.Fatalf("dry-run must not change the file: %q", v)
	}
	jafter, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if len(jafter) != len(jbefore) {
		t.Fatal("dry-run must not journal")
	}
}

// Among same-second candidates, undo picks the last-written row, matching @1 = newest semantics.
func TestUndoSameSecondPicksNewestRow(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	f.capture(t, "tool_a", "first", func() { f.write(t, "f.txt", "v2") })
	f.capture(t, "tool_b", "second", func() { f.write(t, "f.txt", "v3") })

	// Collapse both captures into the same second, preserving file order.
	jp := f.layout.JournalPath(f.work)
	entries, err := journal.ReadMerged(jp, journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jp); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		e.TS = "2026-06-10T00:00:05Z"
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, []string{"--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("undo --dry-run exit %d: %s", code, errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("tool_b")) {
		t.Fatalf("same-second undo should target last-written tool_b, got %q", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("tool_a")) {
		t.Fatalf("same-second undo should not target tool_a, got %q", out.String())
	}
}

// A reclaimed candidate stops undo with an error; it does not silently skip to an older entry (Phase 0 ruling).
func TestUndoReclaimedStops(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	f.capture(t, "tool_rec", "x", func() { f.write(t, "f.txt", "v2") })

	// Reclaim: remove the session shadow repo so its commits no longer resolve.
	if err := os.RemoveAll(f.layout.SessionGitDir(f.work, f.session)); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Undo(f.layout, f.work, nil, &out, &errb); code == 0 {
		t.Fatal("reclaimed candidate should error")
	}
	if !bytes.Contains(errb.Bytes(), []byte("reclaimed")) {
		t.Fatalf("want reclaimed error, got %q", errb.String())
	}
}
