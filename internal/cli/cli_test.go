package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

type fix struct {
	layout  paths.Layout
	work    string
	eng     *snapshot.Engine
	session string
	seq     int
}

func newFix(t *testing.T) *fix {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	layout := paths.New(home)
	return &fix{layout: layout, work: work, eng: snapshot.New(layout, gitx.ExecRunner{}), session: "sess1"}
}

// nextTS returns a monotonically increasing RFC3339 timestamp so captured
// entries have a deterministic ts ordering (real wall-clock captures in the same
// second would otherwise collide and make @N ordering depend on file order).
func (f *fix) nextTS() string {
	f.seq++
	return time.Date(2026, 6, 10, 0, 0, f.seq, 0, time.UTC).Format(time.RFC3339)
}

func (f *fix) write(t *testing.T, name, content string) {
	t.Helper()
	p := filepath.Join(f.work, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// capture records one command into the shadow repo and the journal, returning
// its key.
func (f *fix) capture(t *testing.T, toolID, cmd string, mutate func()) string {
	t.Helper()
	repo, err := f.eng.EnsureRepo(context.Background(), f.work, f.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := f.eng.Pre(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate()
	}
	post, err := f.eng.Post(context.Background(), repo, pre, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := journal.Entry{
		ToolUseID: toolID, SessionID: f.session,
		TS:      f.nextTS(),
		Command: journal.RedactCommand(cmd), PreSHA: pre.PreSHA, PostSHA: post.PostSHA, Status: post.Status,
		Files: post.Files, FilesOmitted: post.FilesOmitted,
	}
	if err := journal.Append(f.layout.JournalPath(f.work), e); err != nil {
		t.Fatal(err)
	}
	return toolID
}

// preCapture records an interrupted command: a real pre snapshot and a disk
// mutation, but no post (orphan pre / pre-only entry).
func (f *fix) preCapture(t *testing.T, toolID, cmd string, mutate func()) string {
	t.Helper()
	repo, err := f.eng.EnsureRepo(context.Background(), f.work, f.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := f.eng.Pre(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate()
	}
	e := journal.Entry{
		ToolUseID: toolID, SessionID: f.session,
		TS:      f.nextTS(),
		Command: journal.RedactCommand(cmd), PreSHA: pre.PreSHA,
	}
	if err := journal.Append(f.layout.JournalPath(f.work), e); err != nil {
		t.Fatal(err)
	}
	return toolID
}

func TestListShowsEntries(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "1")
	f.capture(t, "tool_aaa", "echo hi", func() { f.write(t, "b.txt", "2") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "tool_aaa") || !strings.Contains(s, "protected") {
		t.Fatalf("list output missing entry: %q", s)
	}
}

func TestListEmpty(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "no snapshots") {
		t.Fatalf("want empty message, got %q", out.String())
	}
}

func TestRestoreCLIRoundTrip(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	key := f.capture(t, "tool_x", "edit f", func() { f.write(t, "f.txt", "changed") })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("restore exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(f.work, "f.txt")); string(b) != "original" {
		t.Fatalf("file not restored: %q", b)
	}
	if !strings.Contains(out.String(), "undo with") {
		t.Fatalf("restore output should mention undo: %q", out.String())
	}
	// A restored entry should now exist in the journal.
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	found := false
	for _, e := range entries {
		if e.Status == journal.StatusRestored {
			found = true
		}
	}
	if !found {
		t.Fatal("restored entry not journaled")
	}
}

func TestRestoreCLIUnknownKey(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"nope"}, &out, &errb); code == 0 {
		t.Fatal("unknown key should fail")
	}
	if !strings.Contains(errb.String(), "no entry") {
		t.Fatalf("want 'no entry' message, got %q", errb.String())
	}
}

func TestRestoreCLIOverlappedNeedsForce(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_o", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("overlapped restore should refuse without --force")
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Fatalf("want --force hint, got %q", errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Restore(f.layout, f.work, []string{"--force", key}, &out, &errb); code != 0 {
		t.Fatalf("force restore failed: %s", errb.String())
	}
}

// The overlapped refusal must teach the path filter as a lower-risk alternative
// to --force, and be honest that --force can take concurrent work with it.
func TestRestoreOverlappedGuidesPathFilterAndForce(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_og", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("overlapped restore should refuse without --force")
	}
	msg := errb.String()
	for _, want := range []string{"bashback diff", "<path>", "--force", "concurrent"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("overlapped guidance missing %q: %q", want, msg)
		}
	}
}

// When --force is already in play and the target changed, the refusal must name
// the full --force --3way combo rather than the bare --3way (which would itself
// be refused without --force on this entry).
func TestRestoreTargetChangedWithForceSuggestsCombo(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	key := f.capture(t, "tool_tc", "x", func() { f.write(t, "f.txt", "v1") })
	// Edit the target after the snapshot so the target-changed gate trips.
	f.write(t, "f.txt", "v1-user-edit")

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key, "--force"}, &out, &errb); code == 0 {
		t.Fatal("changed target should refuse a plain --force restore")
	}
	if !strings.Contains(errb.String(), "--force --3way") {
		t.Fatalf("want the --force --3way combo hint, got %q", errb.String())
	}
}

// Flags must take effect after the positional key, the order the usage strings
// advertise (`restore <key> --force`). Go's flag package stops at the first
// non-flag token, so this guards parseFlagsAnywhere against that trap.
func TestRestoreForceAfterKey(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_fa", "x", func() { f.write(t, "f.txt", "b") })
	markOverlapped(t, f, key)

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key, "--force"}, &out, &errb); code != 0 {
		t.Fatalf("--force after key should force through, got refusal: %s", errb.String())
	}
}

// A pre-only entry is labelled in list and refused by diff with a hint.
func TestListAndDiffPreOnly(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	key := f.preCapture(t, "tool_int", "longrun", func() { f.write(t, "f.txt", "half") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "pre-only") || !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("list should flag pre-only/interrupted: %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Diff(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("diff of a pre-only entry should fail")
	}
	if !strings.Contains(errb.String(), "pre-only") || !strings.Contains(errb.String(), "--force") {
		t.Fatalf("diff should explain pre-only and point at restore --force: %q", errb.String())
	}
}

// Pre-only restore via the CLI is refused without --force, then undone.
func TestRestoreCLIPreOnlyNeedsForce(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	key := f.preCapture(t, "tool_int2", "longrun", func() { f.write(t, "f.txt", "half") })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("pre-only restore should refuse without --force")
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Fatalf("want --force hint, got %q", errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Restore(f.layout, f.work, []string{"--force", key}, &out, &errb); code != 0 {
		t.Fatalf("forced pre-only restore failed: %s", errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(f.work, "f.txt")); string(b) != "v1" {
		t.Fatalf("pre-only restore did not revert file: %q", b)
	}
}

// An entry flagged overlapped only by the cross-session read-time computation is
// refused like a stored-flag overlap, and --force lets it through.
func TestRestoreCLICrossSessionOverlapNeedsForce(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a")
	key := f.capture(t, "tool_cs", "x", func() { f.write(t, "f.txt", "b") })

	// Give the captured entry a real time interval, then add an overlapping
	// command from another session. Neither carries a stored overlap flag.
	jp := f.layout.JournalPath(f.work)
	entries, _ := journal.ReadMerged(jp, journal.DefaultKeyer)
	if err := os.Remove(jp); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ToolUseID == key {
			e.TS = "2026-06-10T00:00:05Z"
			e.DurationMS = 5000 // interval [00:00:00, 00:00:05]
		}
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Append(jp, journal.Entry{
		ToolUseID: "other_cmd", SessionID: "sess2",
		TS: "2026-06-10T00:00:08Z", DurationMS: 5000, // [00:00:03, 00:00:08]
		PreSHA: "x", PostSHA: "y", Status: journal.StatusProtected,
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("cross-session overlapped restore should refuse without --force")
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Fatalf("want --force hint, got %q", errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Restore(f.layout, f.work, []string{"--force", key}, &out, &errb); code != 0 {
		t.Fatalf("force restore failed: %s", errb.String())
	}
}

func TestGCCLIDryRun(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	// Old session dir.
	dir := f.layout.SessionGitDir(f.work, "ancient")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	df := filepath.Join(dir, "data")
	os.WriteFile(df, []byte("x"), 0o600)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(df, old, old)

	var out, errb bytes.Buffer
	if code := GC(f.layout, f.work, []string{"--dry-run"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "would reclaim") {
		t.Fatalf("dry-run output: %q", out.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dry-run must not delete")
	}
}

func TestGCAllCLISweepsEveryProject(t *testing.T) {
	f := newFix(t)
	work2 := t.TempDir()
	oldOldSession := func(work, id string) {
		if err := f.layout.EnsureRepoDirs(work); err != nil {
			t.Fatal(err)
		}
		dir := f.layout.SessionGitDir(work, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		df := filepath.Join(dir, "data")
		os.WriteFile(df, []byte("x"), 0o600)
		old := time.Now().Add(-30 * 24 * time.Hour)
		os.Chtimes(df, old, old)
	}
	oldOldSession(f.work, "ancient-1")
	oldOldSession(work2, "ancient-2")

	var out, errb bytes.Buffer
	if code := GC(f.layout, f.work, []string{"--all", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "across 2 project(s)") {
		t.Fatalf("--all should report both projects: %q", out.String())
	}
	// Dry-run: nothing deleted in either project.
	if _, err := os.Stat(f.layout.SessionGitDir(work2, "ancient-2")); err != nil {
		t.Fatal("dry-run --all must not delete")
	}
}

func TestDoctorRuns(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Doctor(f.layout, f.work, nil, &out, &errb)
	if !strings.Contains(out.String(), "git") {
		t.Fatalf("doctor should report git version: %q", out.String())
	}
	if code != 0 {
		t.Logf("doctor reported a problem (may be environmental): %q", out.String())
	}
}

func TestDiffCLI(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "before\n")
	key := f.capture(t, "tool_d", "x", func() { f.write(t, "f.txt", "after\n") })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("diff exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "after") {
		t.Fatalf("diff output should show change: %q", out.String())
	}
}

// diff <k1> <k2> shows the patch between two entries' tree states within a
// session; it equals DiffPatch(tree(A), tree(B)).
func TestDiffTwoKeysPatch(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0\n")
	keyA := f.capture(t, "tool_dA", "a", func() { f.write(t, "f.txt", "v1\n") })
	keyB := f.capture(t, "tool_dB", "b", func() { f.write(t, "f.txt", "v2\n") })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{keyA, keyB}, &out, &errb); code != 0 {
		t.Fatalf("diff A B exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "-v1") || !strings.Contains(got, "+v2") {
		t.Fatalf("two-key patch should show v1->v2: %q", got)
	}
	eA, _, _ := resolveEntry(f.layout, f.work, keyA)
	eB, _, _ := resolveEntry(f.layout, f.work, keyB)
	r := newEngine(f.layout).RepoFor(f.work, eA.SessionID)
	want, err := r.DiffPatch(ctx(), eA.PostSHA, eB.PostSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("two-key patch != DiffPatch(post A, post B):\n got=%q\nwant=%q", got, want)
	}
}

func TestDiffTwoKeysStatAndJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0\n")
	keyA := f.capture(t, "tool_sjA", "a", func() { f.write(t, "f.txt", "v1\n") })
	keyB := f.capture(t, "tool_sjB", "b", func() { f.write(t, "f.txt", "v2\n") })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{keyA, keyB, "--stat"}, &out, &errb); code != 0 {
		t.Fatalf("stat exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "f.txt") || !strings.Contains(out.String(), "changed") {
		t.Fatalf("two-key --stat shape wrong: %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Diff(f.layout, f.work, []string{keyA, keyB, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if m["patch"] == nil {
		t.Fatalf("two-key --json should carry patch: %v", m)
	}
}

func TestDiffTwoKeysCrossSessionRefused(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0\n")
	keyA := f.capture(t, "tool_csA", "a", func() { f.write(t, "f.txt", "v1\n") })
	if err := journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "tool_csB", SessionID: "sess2",
		TS: "2026-06-10T01:00:00Z", PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{keyA, "tool_csB"}, &out, &errb); code == 0 {
		t.Fatal("cross-session diff should refuse")
	}
	if !strings.Contains(errb.String(), "different sessions") {
		t.Fatalf("want cross-session error, got %q", errb.String())
	}
}

// A pre-only (here interrupted) entry contributes its pre snapshot as the tree
// state; a normal entry contributes its post.
func TestDiffTwoKeysPreOnlyUsesPre(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0\n")
	keyA := f.preCapture(t, "tool_poA", "interrupted", func() { f.write(t, "f.txt", "v1\n") })
	keyB := f.capture(t, "tool_poB", "b", func() { f.write(t, "f.txt", "v2\n") })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{keyA, keyB}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	eA, _, _ := resolveEntry(f.layout, f.work, keyA)
	eB, _, _ := resolveEntry(f.layout, f.work, keyB)
	r := newEngine(f.layout).RepoFor(f.work, eA.SessionID)
	want, err := r.DiffPatch(ctx(), eA.PreSHA, eB.PostSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("pre-only A should use its pre snapshot:\n got=%q\nwant=%q", got, want)
	}
}

// A second positional that is a path (not an entry) keeps the single-key
// path-filter behavior, unchanged by the two-key feature.
func TestDiffSingleKeyPathStillWorks(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "before\n")
	f.write(t, "b.txt", "x\n")
	key := f.capture(t, "tool_spf", "x", func() {
		f.write(t, "a.txt", "after\n")
		f.write(t, "b.txt", "y\n")
	})
	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key, "a.txt"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "a.txt") || strings.Contains(out.String(), "b.txt") {
		t.Fatalf("path filter should show only a.txt: %q", out.String())
	}
}

// A second positional shorter than the 4-char key-prefix floor must still be
// usable as a path filter (src/, lib/ … are common dir names); the floor only
// guards key resolution.
func TestDiffShortPathFilterFallsBack(t *testing.T) {
	f := newFix(t)
	if err := os.MkdirAll(filepath.Join(f.work, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(t, "src/x.txt", "before\n")
	f.write(t, "b.txt", "x\n")
	key := f.capture(t, "tool_spf2", "x", func() {
		f.write(t, "src/x.txt", "after\n")
		f.write(t, "b.txt", "y\n")
	})
	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key, "src"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "x.txt") || strings.Contains(out.String(), "b.txt") {
		t.Fatalf("short path filter should show only src/x.txt: %q", out.String())
	}
}

// A too-short second arg matching no file yields an empty filtered diff (exit
// 0), not a hard error; the first positional keeps the 4-char floor.
func TestDiffTooShortRefBehavior(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "before\n")
	key := f.capture(t, "tool_ts", "x", func() { f.write(t, "a.txt", "after\n") })
	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key, "zz"}, &out, &errb); code != 0 {
		t.Fatalf("no-match short filter must not error: exit %d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "a.txt") {
		t.Fatalf("filter zz must exclude a.txt: %q", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := Diff(f.layout, f.work, []string{"zz"}, &out, &errb); code == 0 {
		t.Fatal("too-short first arg must still be refused")
	}
	if !strings.Contains(errb.String(), "too short") {
		t.Fatalf("expected too-short error, got: %s", errb.String())
	}
}

func markOverlapped(t *testing.T, f *fix, key string) {
	t.Helper()
	entries, err := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite journal with the entry marked overlapped.
	jp := f.layout.JournalPath(f.work)
	if err := os.Remove(jp); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ToolUseID == key {
			e.Overlapped = true
		}
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}
}
