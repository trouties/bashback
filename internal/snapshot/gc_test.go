package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/paths"
)

func gcEngine(t *testing.T) (*Engine, string, time.Time) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	e := New(paths.New(home), gitx.ExecRunner{})
	e.Now = func() time.Time { return now }
	if err := e.Layout.EnsureRepoDirs(work); err != nil {
		t.Fatal(err)
	}
	return e, work, now
}

// makeSession creates a fake session repo dir with a sized file aged to mtime.
func makeSession(t *testing.T, e *Engine, work, id string, size int, mtime time.Time) {
	t.Helper()
	dir := e.Layout.SessionGitDir(work, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "data")
	if err := os.WriteFile(f, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func sessionExists(e *Engine, work, id string) bool {
	_, err := os.Stat(e.Layout.SessionGitDir(work, id))
	return err == nil
}

func TestGCExpiresOldKeepsFreshAndActive(t *testing.T) {
	e, work, now := gcEngine(t)
	makeSession(t, e, work, "old", 10, now.Add(-30*24*time.Hour))
	makeSession(t, e, work, "fresh", 10, now.Add(-1*time.Hour))
	makeSession(t, e, work, "active-old", 10, now.Add(-30*24*time.Hour))

	// journal must survive GC untouched.
	jpath := e.Layout.JournalPath(work)
	if err := os.WriteFile(jpath, []byte(`{"v":1,"tool_use_id":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, ActiveSessions: map[string]bool{"active-old": true}})
	if err != nil {
		t.Fatal(err)
	}
	if sessionExists(e, work, "old") {
		t.Error("expired session should be removed")
	}
	if !sessionExists(e, work, "fresh") {
		t.Error("fresh session must survive")
	}
	if !sessionExists(e, work, "active-old") {
		t.Error("active session must never be cleaned despite age")
	}
	if _, err := os.Stat(jpath); err != nil {
		t.Error("journal must never be deleted")
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "old" {
		t.Fatalf("report.Removed = %v, want [old]", rep.Removed)
	}
}

func TestGCAllAcrossProjects(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	e := New(paths.New(home), gitx.ExecRunner{})
	e.Now = func() time.Time { return now }
	work1, work2 := t.TempDir(), t.TempDir()
	for _, w := range []string{work1, work2} {
		if err := e.Layout.EnsureRepoDirs(w); err != nil {
			t.Fatal(err)
		}
	}
	// meta.json on project 1 so its report carries the original path label.
	if err := e.Layout.WriteMeta(work1, paths.Meta{OriginalPath: work1}); err != nil {
		t.Fatal(err)
	}
	makeSession(t, e, work1, "p1-old", 10, now.Add(-30*24*time.Hour))
	makeSession(t, e, work1, "p1-fresh", 10, now.Add(-1*time.Hour))
	makeSession(t, e, work1, "p1-active", 10, now.Add(-30*24*time.Hour))
	makeSession(t, e, work2, "p2-old", 10, now.Add(-30*24*time.Hour))

	// journal must survive on each project.
	for _, w := range []string{work1, work2} {
		if err := os.WriteFile(e.Layout.JournalPath(w), []byte(`{"v":1,"tool_use_id":"x"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reports, err := e.GCAll(GCOpts{OlderThan: 14 * 24 * time.Hour, ActiveSessions: map[string]bool{"p1-active": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("want a report per project, got %d", len(reports))
	}
	if sessionExists(e, work1, "p1-old") || sessionExists(e, work2, "p2-old") {
		t.Error("expired sessions in every project should be reclaimed")
	}
	if !sessionExists(e, work1, "p1-fresh") {
		t.Error("fresh session must survive")
	}
	if !sessionExists(e, work1, "p1-active") {
		t.Error("active session must never be reclaimed")
	}
	for _, w := range []string{work1, work2} {
		if _, err := os.Stat(e.Layout.JournalPath(w)); err != nil {
			t.Errorf("journal for %s must never be deleted", w)
		}
	}
	var labelled bool
	for _, p := range reports {
		if p.Project == work1 {
			labelled = true
		}
	}
	if !labelled {
		t.Errorf("report should label project 1 by its original path %q, got %+v", work1, reports)
	}
}

func TestGCDryRunRemovesNothing(t *testing.T) {
	e, work, now := gcEngine(t)
	makeSession(t, e, work, "old", 10, now.Add(-30*24*time.Hour))

	rep, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionExists(e, work, "old") {
		t.Error("dry-run must not delete")
	}
	if len(rep.Removed) != 1 {
		t.Fatalf("dry-run should still report 1 removal, got %v", rep.Removed)
	}
}

func TestGCSoftCapEvictsOldestFirst(t *testing.T) {
	e, work, now := gcEngine(t)
	// Three within retention, total 3000 bytes; cap at 1500 -> evict oldest two.
	makeSession(t, e, work, "s-oldest", 1000, now.Add(-3*time.Hour))
	makeSession(t, e, work, "s-mid", 1000, now.Add(-2*time.Hour))
	makeSession(t, e, work, "s-newest", 1000, now.Add(-1*time.Hour))

	rep, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, SoftCapBytes: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if sessionExists(e, work, "s-oldest") || sessionExists(e, work, "s-mid") {
		t.Errorf("soft cap should evict oldest two, removed=%v", rep.Removed)
	}
	if !sessionExists(e, work, "s-newest") {
		t.Error("newest should be kept under soft cap")
	}
}

func TestGCSoftCapNeverEvictsActive(t *testing.T) {
	e, work, now := gcEngine(t)
	makeSession(t, e, work, "active", 5000, now.Add(-3*time.Hour))
	makeSession(t, e, work, "idle", 5000, now.Add(-1*time.Hour))

	_, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, SoftCapBytes: 1, ActiveSessions: map[string]bool{"active": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionExists(e, work, "active") {
		t.Error("active session must survive even when over the soft cap")
	}
}

func TestSoftCapSparesRecentlyWrittenSessions(t *testing.T) {
	e, work, now := gcEngine(t)
	// Both over the cap together; cap smaller than either alone. A has a fresh
	// write (10min), B is 3 days old. B must go; A stays even though removing A
	// would also satisfy the cap.
	makeSession(t, e, work, "A-fresh", 1000, now.Add(-10*time.Minute))
	makeSession(t, e, work, "B-stale", 1000, now.Add(-72*time.Hour))

	_, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, SoftCapBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	if sessionExists(e, work, "B-stale") {
		t.Error("stale session should be evicted under the soft cap")
	}
	if !sessionExists(e, work, "A-fresh") {
		t.Error("recently-written session must be spared even when over the cap")
	}
}

func TestSoftCapAlwaysKeepsNewestSession(t *testing.T) {
	e, work, now := gcEngine(t)
	makeSession(t, e, work, "only", 5000, now.Add(-48*time.Hour))

	_, err := e.GC(work, GCOpts{OlderThan: 14 * 24 * time.Hour, SoftCapBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionExists(e, work, "only") {
		t.Error("the newest (here only) session of a project must never be evicted by the soft cap")
	}
}
