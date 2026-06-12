package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// EnsureRepo writes perf config only when it creates the repo; it never silently
// migrates an existing shadow repo's config.
func TestEnsureRepoLeavesExistingConfig(t *testing.T) {
	h := newHarness(t)
	if _, err := h.e.EnsureRepo(ctx(), h.work, h.session); err != nil {
		t.Fatal(err)
	}
	gitDir := h.e.Layout.SessionGitDir(h.work, h.session)
	gitConfig := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"--git-dir", gitDir, "config"}, args...)...).Output()
		if err != nil {
			t.Fatalf("git config %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	// Simulate a repo created before perf config landed: downgrade a value.
	if err := exec.Command("git", "--git-dir", gitDir, "config", "index.version", "2").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.EnsureRepo(ctx(), h.work, h.session); err != nil {
		t.Fatal(err)
	}
	if got := gitConfig("--get", "index.version"); got != "2" {
		t.Fatalf("existing index.version was rewritten to %q; must not silently migrate", got)
	}
}

func ctx() context.Context { return context.Background() }

// statusCounter wraps a real Runner and counts full-tree `status` invocations, so
// a test can assert the hot path's probe budget without faking git behavior.
type statusCounter struct {
	inner gitx.Runner
	mu    sync.Mutex
	count int
}

func (s *statusCounter) Run(c context.Context, args []string, opts gitx.RunOpts) (gitx.Result, error) {
	for _, a := range args {
		if a == "status" {
			s.mu.Lock()
			s.count++
			s.mu.Unlock()
			break
		}
	}
	return s.inner.Run(c, args, opts)
}

func (s *statusCounter) reset() { s.mu.Lock(); s.count = 0; s.mu.Unlock() }
func (s *statusCounter) get() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// The post path takes exactly one full-tree `status --porcelain` probe: the
// oversized-scan ChangedPaths. The commit decision is an index-vs-HEAD probe, not
// a second status.
func TestSnapSingleStatusProbe(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	sc := &statusCounter{inner: gitx.ExecRunner{}}
	e := New(paths.New(home), sc)
	repo, err := e.EnsureRepo(ctx(), work, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	pre, err := e.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc.reset()
	if _, err := e.Post(ctx(), repo, pre, nil); err != nil {
		t.Fatal(err)
	}
	if got := sc.get(); got != 1 {
		t.Fatalf("post path issued %d full-tree status probes, want 1", got)
	}
}

type harness struct {
	e       *Engine
	work    string
	session string
	n       int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	return &harness{e: New(paths.New(home), gitx.ExecRunner{}), work: work, session: "sess1"}
}

func (h *harness) write(t *testing.T, name, content string) {
	t.Helper()
	p := filepath.Join(h.work, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) read(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(h.work, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// capture runs one simulated command: pre snapshot, mutate, post snapshot, and
// returns the assembled journal entry.
func (h *harness) capture(t *testing.T, force []string, mutate func()) journal.Entry {
	t.Helper()
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := h.e.Pre(ctx(), repo, force)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	if mutate != nil {
		mutate()
	}
	post, err := h.e.Post(ctx(), repo, pre, force)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	h.n++
	return journal.Entry{
		ToolUseID:    fmt.Sprintf("toolu_%d", h.n),
		SessionID:    h.session,
		PreSHA:       pre.PreSHA,
		PostSHA:      post.PostSHA,
		Status:       post.Status,
		Note:         pre.Note + post.Note,
		Files:        post.Files,
		FilesOmitted: post.FilesOmitted,
	}
}

func TestFastPathStatuses(t *testing.T) {
	h := newHarness(t)
	h.write(t, "a.txt", "v1")
	first := h.capture(t, nil, nil)
	if first.Status != journal.StatusProtected {
		t.Fatalf("first capture status = %s, want protected", first.Status)
	}

	// No change at all -> skipped_no_change, post == pre.
	noop := h.capture(t, nil, nil)
	if noop.Status != journal.StatusSkippedNoChange {
		t.Fatalf("noop status = %s, want skipped_no_change", noop.Status)
	}
	if noop.PreSHA != noop.PostSHA {
		t.Fatalf("skipped_no_change must have post==pre")
	}

	// Pre fast-path hit but the command creates a change -> protected.
	changed := h.capture(t, nil, func() { h.write(t, "b.txt", "new") })
	if changed.Status != journal.StatusProtected {
		t.Fatalf("changed status = %s, want protected", changed.Status)
	}
	if changed.PreSHA == changed.PostSHA {
		t.Fatal("a real change should advance post sha")
	}
}

func TestRestoreModifiedDeletedAdded(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	h.write(t, "doomed/inner.txt", "data")

	entry := h.capture(t, nil, func() {
		os.RemoveAll(filepath.Join(h.work, "doomed"))
		h.write(t, "keep.txt", "v2")
		h.write(t, "created.txt", "added by command")
	})
	if entry.Status != journal.StatusProtected {
		t.Fatalf("status = %s", entry.Status)
	}

	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v1" {
		t.Errorf("keep.txt = %q, want v1 (reverted)", v)
	}
	if v, ok := h.read("doomed/inner.txt"); !ok || v != "data" {
		t.Errorf("deleted dir not restored: %q ok=%v", v, ok)
	}
	if _, ok := h.read("created.txt"); ok {
		t.Error("command-created file should be removed by restore")
	}
	if _, err := os.Stat(filepath.Join(h.work, "doomed")); err != nil {
		t.Errorf("restored dir missing: %v", err)
	}
}

func TestRestoreIsUndoable(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "original")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "command-edit") })

	restored, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := h.read("f.txt"); v != "original" {
		t.Fatalf("after restore = %q, want original", v)
	}
	if restored.Status != journal.StatusRestored {
		t.Fatalf("restored entry status = %s", restored.Status)
	}
	// Undo the restore -> back to the command's result.
	if _, err := h.e.Restore(ctx(), h.work, restored, RestoreOpts{}); err != nil {
		t.Fatalf("undo restore: %v", err)
	}
	if v, _ := h.read("f.txt"); v != "command-edit" {
		t.Fatalf("after undo = %q, want command-edit", v)
	}
}

func TestRestoreRefusesTargetChangedThenThreeWay(t *testing.T) {
	h := newHarness(t)
	// Command change (top) and user edit (bottom) are far apart so the 3way
	// merge is non-overlapping.
	const pre = "top\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom\n"
	const post = "top-cmd\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom\n"
	h.write(t, "f.txt", pre)
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", post) })

	// User edits the far-away bottom line after the snapshot.
	h.write(t, "f.txt", "top-cmd\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom-user\n")

	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != ErrTargetChanged {
		t.Fatalf("want ErrTargetChanged, got %v", err)
	}
	// --3way undoes the top change while preserving the user's bottom edit.
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{ThreeWay: true}); err != nil {
		t.Fatalf("3way restore: %v", err)
	}
	v, _ := h.read("f.txt")
	if want := "top\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom-user\n"; v != want {
		t.Fatalf("3way result = %q, want %q", v, want)
	}
}

func TestRestoreThreeWayConflictMarkers(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "alpha\n")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "beta\n") })

	// User overwrites the same line differently after the snapshot.
	h.write(t, "f.txt", "gamma\n")

	restored, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{ThreeWay: true})
	if err != nil {
		t.Fatalf("3way restore: %v", err)
	}
	v, _ := h.read("f.txt")
	if !contains(v, "<<<<<<<") || !contains(v, ">>>>>>>") {
		t.Fatalf("expected conflict markers, got %q", v)
	}
	if restored.Note == "" {
		t.Error("conflict should be noted in restored entry")
	}
}

func TestRestoreOverlappedRefused(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "x")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "y") })
	entry.Overlapped = true

	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != ErrOverlapped {
		t.Fatalf("want ErrOverlapped, got %v", err)
	}
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{Force: true}); err != nil {
		t.Fatalf("force restore of overlapped: %v", err)
	}
}

// preCapture simulates an interrupted command (Esc): a pre snapshot, a mutation
// on disk, but no post — the daemon orphan-pre case. Returns the pre-only entry.
func (h *harness) preCapture(t *testing.T, mutate func()) journal.Entry {
	t.Helper()
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := h.e.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	if mutate != nil {
		mutate()
	}
	h.n++
	return journal.Entry{
		ToolUseID: fmt.Sprintf("toolu_%d", h.n),
		SessionID: h.session,
		PreSHA:    pre.PreSHA,
		// No PostSHA / Status: this is an orphan pre.
	}
}

// A pre-only orphan (Esc-interrupted) is refused by default, then undone with
// --force by lazily snapshotting the work-tree as the post.
func TestRestorePreOnlyLazyPost(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	h.write(t, "doomed/inner.txt", "data")

	entry := h.preCapture(t, func() {
		os.RemoveAll(filepath.Join(h.work, "doomed"))
		h.write(t, "keep.txt", "v2")
		h.write(t, "created.txt", "half-finished")
	})

	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != ErrPreOnly {
		t.Fatalf("pre-only restore without --force should be ErrPreOnly, got %v", err)
	}
	// --force: lazy-snapshot the work-tree as post and undo.
	restored, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{Force: true})
	if err != nil {
		t.Fatalf("forced pre-only restore: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v1" {
		t.Errorf("keep.txt = %q, want v1", v)
	}
	if v, ok := h.read("doomed/inner.txt"); !ok || v != "data" {
		t.Errorf("interrupted deletion not restored: %q ok=%v", v, ok)
	}
	if _, ok := h.read("created.txt"); ok {
		t.Error("half-finished created file should be removed")
	}
	if restored.Status != journal.StatusRestored || !contains(restored.Note, "pre-only") {
		t.Fatalf("restored entry should note the lazy pre-only post: %+v", restored)
	}
	// The restore is itself undoable.
	if _, err := h.e.Restore(ctx(), h.work, restored, RestoreOpts{}); err != nil {
		t.Fatalf("undo of pre-only restore: %v", err)
	}
}

// A permission-denied orphan (pre, but no disk side effect) is harmless —
// --force finds nothing to restore.
func TestRestorePreOnlyNoSideEffect(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "v1")
	entry := h.preCapture(t, nil)

	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{Force: true}); err != ErrNothingToRestore {
		t.Fatalf("pre-only with no side effect should be ErrNothingToRestore, got %v", err)
	}
}

func TestRestoreReclaimedSnapshot(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "x")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "y") })
	entry.PreSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	_, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{})
	if err == nil || !isReclaimed(err) {
		t.Fatalf("want ErrSnapshotReclaimed, got %v", err)
	}
}

func TestRestorePathFilter(t *testing.T) {
	h := newHarness(t)
	h.write(t, "a.txt", "a1")
	h.write(t, "b.txt", "b1")
	entry := h.capture(t, nil, func() {
		h.write(t, "a.txt", "a2")
		h.write(t, "b.txt", "b2")
	})

	// Restore only a.txt.
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{Paths: []string{"a.txt"}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.read("a.txt"); v != "a1" {
		t.Errorf("a.txt = %q, want a1", v)
	}
	if v, _ := h.read("b.txt"); v != "b2" {
		t.Errorf("b.txt = %q, want b2 (untouched)", v)
	}

	// Path with no changes -> ErrNothingToRestore.
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{Paths: []string{"nonexistent.txt"}}); err != ErrNothingToRestore {
		t.Fatalf("want ErrNothingToRestore, got %v", err)
	}
}

func TestOversizedFileExcluded(t *testing.T) {
	h := newHarness(t)
	h.e.MaxFileBytes = 16
	h.write(t, "base.txt", "x")
	h.write(t, "model.bin", "this is way over the sixteen byte cap")

	entry := h.capture(t, nil, nil)
	if !contains(entry.Note, "oversized") {
		t.Fatalf("note should mention oversized exclusion: %q", entry.Note)
	}
	// Fast-path must still work (E7): a second no-op capture is skipped.
	noop := h.capture(t, nil, nil)
	if noop.Status != journal.StatusSkippedNoChange {
		t.Fatalf("after exclude, fast-path broken: %s", noop.Status)
	}
}

func TestUnreadableFileRetry(t *testing.T) {
	h := newHarness(t)
	h.write(t, "ok.txt", "fine")
	h.write(t, "secret.txt", "locked")
	if err := os.Chmod(filepath.Join(h.work, "secret.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.work, "secret.txt"), 0o644) })

	entry := h.capture(t, nil, nil)
	if entry.Status != journal.StatusProtected {
		t.Fatalf("status = %s, want protected after exclude+retry", entry.Status)
	}
	if !contains(entry.Note, "unreadable") {
		t.Fatalf("note should record excluded unreadable path: %q", entry.Note)
	}
}

func TestNestedRepoGitlinkNote(t *testing.T) {
	h := newHarness(t)
	h.write(t, "top.txt", "x")
	// Create a nested real git repo.
	nested := filepath.Join(h.work, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	r := gitx.NewRepo(filepath.Join(nested, ".git"), nested, gitx.ExecRunner{})
	if err := r.Init(ctx()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.AddAll(ctx()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx(), "nested"); err != nil {
		t.Fatal(err)
	}

	entry := h.capture(t, nil, nil)
	if !contains(entry.Note, "gitlink") {
		t.Fatalf("note should flag nested gitlink blind spot: %q", entry.Note)
	}
}

func TestForceIncludeRecoversIgnoredFile(t *testing.T) {
	h := newHarness(t)
	// Set up force_include before the first snapshot.
	if err := h.e.Layout.EnsureRepoDirs(h.work); err != nil {
		t.Fatal(err)
	}
	must(t, h.e.Layout.WriteMeta(h.work, paths.Meta{OriginalPath: h.work, ForceInclude: []string{".env"}}))

	h.write(t, ".gitignore", ".env\n")
	h.write(t, ".env", "SECRET=keep")

	entry := h.capture(t, []string{".env"}, func() {
		os.Remove(filepath.Join(h.work, ".env"))
	})
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if v, ok := h.read(".env"); !ok || v != "SECRET=keep" {
		t.Fatalf("force-included .env not restored: %q ok=%v", v, ok)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func isReclaimed(err error) bool {
	return err != nil && contains(err.Error(), "reclaimed")
}

var _ = time.Now

// A stale shadow index.lock (orphan from a killed git) must be recovered so the
// session keeps taking snapshots instead of failing forever.
func TestSnapRecoversStaleIndexLock(t *testing.T) {
	h := newHarness(t)
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Pre(ctx(), repo, nil); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo.GitDir, "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	h.write(t, "f.txt", "x")
	if _, err := h.e.Pre(ctx(), repo, nil); err != nil {
		t.Fatalf("Pre with stale lock: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("stale index.lock not removed")
	}
}

// A fresh index.lock may be a live cross-process writer: snap must not remove it.
func TestSnapLeavesFreshIndexLock(t *testing.T) {
	h := newHarness(t)
	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Pre(ctx(), repo, nil); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo.GitDir, "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(lock, now, now); err != nil {
		t.Fatal(err)
	}
	h.write(t, "f.txt", "x")
	if _, err := h.e.Pre(ctx(), repo, nil); err == nil {
		t.Fatal("Pre should fail on a fresh index.lock")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("fresh index.lock must survive")
	}
}
