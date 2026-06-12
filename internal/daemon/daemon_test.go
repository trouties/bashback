package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

type testDaemon struct {
	d      *Daemon
	layout paths.Layout
	work   string
}

// build constructs a daemon and listener without serving, so a test can inject
// a fake clock (Engine.Now) before any worker goroutine reads it.
func build(t *testing.T) (*testDaemon, net.Listener) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	layout := paths.New(home)
	eng := snapshot.New(layout, gitx.ExecRunner{})
	d := New(eng, layout, "sess1", nil)
	d.IdleTimeout = time.Hour
	ln, err := Listen(layout, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	return &testDaemon{d: d, layout: layout, work: work}, ln
}

func (td *testDaemon) serve(t *testing.T, ln net.Listener) {
	t.Helper()
	go td.d.Serve(ln)
	t.Cleanup(td.d.Stop)
}

func start(t *testing.T) *testDaemon {
	t.Helper()
	td, ln := build(t)
	td.serve(t, ln)
	return td
}

// startClock builds a daemon with the fake clock installed before serving, then
// serves it. Returns the daemon and the clock for the test to advance.
func startClock(t *testing.T, rfc3339 string, ttl time.Duration) (*testDaemon, *clock) {
	t.Helper()
	td, ln := build(t)
	c := newClock(rfc3339)
	td.d.Engine.Now = c.now
	td.d.StaleTTL = ttl
	td.serve(t, ln)
	return td, c
}

func (td *testDaemon) call(t *testing.T, req Request) Response {
	t.Helper()
	if req.Workdir == "" {
		req.Workdir = td.work
	}
	if req.SessionID == "" {
		req.SessionID = "sess1"
	}
	c, err := net.DialTimeout("unix", td.layout.SocketPath("sess1"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func (td *testDaemon) writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(td.work, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (td *testDaemon) journalEntries(t *testing.T) map[string]journal.Entry {
	t.Helper()
	entries, err := journal.ReadMerged(td.layout.JournalPath(td.work), journal.ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]journal.Entry{}
	for _, e := range entries {
		m[e.ToolUseID] = e
	}
	return m
}

func TestPrePostRoundTrip(t *testing.T) {
	td := start(t)
	if r := td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "touch f"}); !r.OK {
		t.Fatalf("pre failed: %+v", r)
	}
	td.writeFile(t, "f", "created by command")
	r := td.call(t, Request{Op: OpPost, ToolUseID: "t1", Command: "touch f"})
	if !r.OK || r.Status != string(journal.StatusProtected) {
		t.Fatalf("post = %+v, want protected", r)
	}
	e := td.journalEntries(t)["t1"]
	if e.Status != journal.StatusProtected || e.PreSHA == "" || e.PostSHA == "" {
		t.Fatalf("journal entry = %+v", e)
	}
}

// Zero-loss: many concurrent clients, all snapshots recorded, none lost to
// index.lock races.
func TestConcurrentZeroLoss(t *testing.T) {
	td := start(t)
	const n = 12
	var wg sync.WaitGroup
	results := make([]Response, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "tool" + itoa(i)
			td.call(t, Request{Op: OpPre, ToolUseID: id, Command: "cmd" + itoa(i)})
			td.writeFile(t, "file"+itoa(i), "data"+itoa(i))
			results[i] = td.call(t, Request{Op: OpPost, ToolUseID: id, Command: "cmd" + itoa(i)})
		}(i)
	}
	wg.Wait()

	entries := td.journalEntries(t)
	if len(entries) != n {
		t.Fatalf("journal has %d entries, want %d (snapshots lost)", len(entries), n)
	}
	for id, e := range entries {
		if e.Status == journal.StatusUnprotectedError {
			t.Errorf("%s: unprotected error (lock race?): %s", id, e.Note)
		}
	}
}

// Overlap: when a second command's pre arrives while the first is still open,
// both entries must be flagged overlapped.
func TestOverlapDetection(t *testing.T) {
	td := start(t)
	// A and B open concurrently (pre A, pre B) then close (post A, post B).
	td.call(t, Request{Op: OpPre, ToolUseID: "A", Command: "a"})
	td.call(t, Request{Op: OpPre, ToolUseID: "B", Command: "b"})
	td.writeFile(t, "a.txt", "a")
	td.call(t, Request{Op: OpPost, ToolUseID: "A", Command: "a"})
	td.writeFile(t, "b.txt", "b")
	td.call(t, Request{Op: OpPost, ToolUseID: "B", Command: "b"})

	e := td.journalEntries(t)
	if !e["A"].Overlapped || !e["B"].Overlapped {
		t.Fatalf("both A and B should be overlapped: A=%v B=%v", e["A"].Overlapped, e["B"].Overlapped)
	}
}

func TestSequentialNotOverlapped(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "A", Command: "a"})
	td.writeFile(t, "a.txt", "a")
	td.call(t, Request{Op: OpPost, ToolUseID: "A", Command: "a"})
	td.call(t, Request{Op: OpPre, ToolUseID: "B", Command: "b"})
	td.writeFile(t, "b.txt", "b")
	td.call(t, Request{Op: OpPost, ToolUseID: "B", Command: "b"})

	e := td.journalEntries(t)
	if e["A"].Overlapped || e["B"].Overlapped {
		t.Fatalf("sequential commands must not be overlapped")
	}
}

// A backgrounded command's post is taken at the moment it goes to the
// background, so later writes are unprotected: the entry must carry the
// background note regardless of whether the snapshot caught any change yet.
func TestBackgroundCommandFlagged(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "bg", Command: "long &", Background: true})
	// No file written: the work happens after backgrounding.
	r := td.call(t, Request{Op: OpPost, ToolUseID: "bg", Command: "long &", Background: true})
	if !r.OK {
		t.Fatalf("post failed: %+v", r)
	}
	e := td.journalEntries(t)["bg"]
	if !contains(e.Note, "background") {
		t.Fatalf("background entry note = %q, want a background marker", e.Note)
	}
}

// A backgrounded command's post entry records the forwarded backgroundTaskId as
// bg_task_id, the lookup key for the later bgfinal capture.
func TestBackgroundEntryRecordsTaskID(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "bg", Command: "long &", Background: true})
	r := td.call(t, Request{Op: OpPost, ToolUseID: "bg", Command: "long &", Background: true, BgTaskID: "b32gd3xrm"})
	if !r.OK {
		t.Fatalf("post failed: %+v", r)
	}
	e := td.journalEntries(t)["bg"]
	if e.BgTaskID != "b32gd3xrm" {
		t.Fatalf("background entry bg_task_id = %q, want b32gd3xrm", e.BgTaskID)
	}
}

// The OpBgFinal request captures a backgrounded command's later writes as a
// bgfinal entry through the daemon worker.
func TestBgFinalViaDaemon(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "bg", Command: "long &", Background: true})
	td.writeFile(t, "out.log", "")
	td.call(t, Request{Op: OpPost, ToolUseID: "bg", Command: "long &", Background: true, BgTaskID: "taskZ"})
	// Background command writes after backgrounding.
	td.writeFile(t, "out.log", "done\n")

	r := td.call(t, Request{Op: OpBgFinal, BgTaskID: "taskZ"})
	if !r.OK || r.Key != "bgfinal_bg" {
		t.Fatalf("bgfinal response = %+v, want OK bgfinal_bg", r)
	}
	e := td.journalEntries(t)["bgfinal_bg"]
	if e.PostSHA == "" || e.Status != journal.StatusProtected {
		t.Fatalf("bgfinal entry = %+v", e)
	}
}

func TestCommandRedactedInJournal(t *testing.T) {
	td := start(t)
	secret := `curl -H "Authorization: Bearer sk-supersecret"`
	td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: secret})
	td.writeFile(t, "f", "x")
	td.call(t, Request{Op: OpPost, ToolUseID: "t1", Command: secret})
	cmd := td.journalEntries(t)["t1"].Command
	if contains(cmd, "sk-supersecret") {
		t.Fatalf("secret leaked into journal: %q", cmd)
	}
}

// The pre half-row lands as soon as pre is handled, before any post — so an Esc
// interrupt leaves a recoverable pre_sha on disk, not stranded in memory.
func TestPreHalfRowWritten(t *testing.T) {
	td := start(t)
	if r := td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "touch f"}); !r.OK {
		t.Fatalf("pre failed: %+v", r)
	}
	e := td.journalEntries(t)["t1"]
	if e.PreSHA == "" {
		t.Fatalf("pre half-row missing pre_sha before post: %+v", e)
	}
	if e.PostSHA != "" || e.Status != "" {
		t.Fatalf("pre-only entry should have no post/status yet: %+v", e)
	}
}

// Pre half-row + post full row merge to exactly one entry, no duplication.
func TestPrePostMergeNoDuplicate(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "touch f"})
	td.writeFile(t, "f", "x")
	td.call(t, Request{Op: OpPost, ToolUseID: "t1", Command: "touch f"})

	entries := td.journalEntries(t)
	if len(entries) != 1 {
		t.Fatalf("want 1 merged entry, got %d", len(entries))
	}
	e := entries["t1"]
	if e.PreSHA == "" || e.PostSHA == "" || e.Status != journal.StatusProtected {
		t.Fatalf("merged entry incomplete: %+v", e)
	}
}

// A fast-path pre (clean work-tree) still records a pre_sha (reused HEAD).
func TestPreHalfRowFastPathReusesSHA(t *testing.T) {
	td := start(t)
	// First command establishes a baseline commit.
	td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "a"})
	td.writeFile(t, "a.txt", "a")
	td.call(t, Request{Op: OpPost, ToolUseID: "t1", Command: "a"})
	// Second pre hits the fast-path (clean tree) and must still carry a pre_sha.
	td.call(t, Request{Op: OpPre, ToolUseID: "t2", Command: "b"})
	if e := td.journalEntries(t)["t2"]; e.PreSHA == "" {
		t.Fatalf("fast-path pre half-row must reuse a snapshot sha: %+v", e)
	}
}

// An orphan pre swept past the TTL stops poisoning later commands. Replays the
// "one Esc poisons the whole session" regression with a fake clock.
func TestOrphanSweepStopsOverlapPoisoning(t *testing.T) {
	td, clock := startClock(t, "2026-06-10T00:00:00Z", 15*time.Minute)

	// An orphan pre (Esc interrupt: pre, no post).
	td.call(t, Request{Op: OpPre, ToolUseID: "orphan", Command: "longrun"})

	// Before the TTL elapses, a new command overlaps the still-open orphan.
	clock.advance(time.Minute)
	td.call(t, Request{Op: OpPre, ToolUseID: "early", Command: "x"})
	td.writeFile(t, "x", "x")
	td.call(t, Request{Op: OpPost, ToolUseID: "early", Command: "x"})
	if !td.journalEntries(t)["early"].Overlapped {
		t.Fatal("within the TTL window the orphan still marks overlap (honest)")
	}

	// After the TTL the sweep orphans the pre; later commands are clean again.
	clock.advance(20 * time.Minute)
	td.d.sweepOpen()
	for i := 0; i < 5; i++ {
		id := "after" + itoa(i)
		td.call(t, Request{Op: OpPre, ToolUseID: id, Command: id})
		td.writeFile(t, id, id)
		td.call(t, Request{Op: OpPost, ToolUseID: id, Command: id})
		if td.journalEntries(t)[id].Overlapped {
			t.Fatalf("%s wrongly overlapped after orphan was swept", id)
		}
	}
	// The orphan itself is annotated as interrupted.
	if e := td.journalEntries(t)["orphan"]; e.Note == "" || e.PostSHA != "" {
		t.Fatalf("orphan should be a noted pre-only entry: %+v", e)
	}
}

// TTL-window concurrency still marks overlap (no overlap-detection regression).
func TestSweepKeepsWithinTTLOverlap(t *testing.T) {
	td, clock := startClock(t, "2026-06-10T00:00:00Z", 15*time.Minute)

	td.call(t, Request{Op: OpPre, ToolUseID: "A", Command: "a"})
	clock.advance(time.Second)
	td.call(t, Request{Op: OpPre, ToolUseID: "B", Command: "b"})
	td.d.sweepOpen() // both well within TTL: nothing swept
	td.writeFile(t, "a.txt", "a")
	td.call(t, Request{Op: OpPost, ToolUseID: "A", Command: "a"})
	td.writeFile(t, "b.txt", "b")
	td.call(t, Request{Op: OpPost, ToolUseID: "B", Command: "b"})

	e := td.journalEntries(t)
	if !e["A"].Overlapped || !e["B"].Overlapped {
		t.Fatalf("within-TTL concurrency must still be overlapped: A=%v B=%v", e["A"].Overlapped, e["B"].Overlapped)
	}
}

// Shutdown flushes every still-open pre with an interrupted note.
func TestExitFlushOrphansOpen(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "stuck", Command: "neverposts"})
	td.d.Stop()
	// Give Serve's exit path a moment to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if td.journalEntries(t)["stuck"].Note != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := td.journalEntries(t)["stuck"]; e.Note == "" {
		t.Fatalf("exit flush should annotate the open orphan: %+v", e)
	}
}

func TestIdleExitRunsGC(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	layout := paths.New(home)
	eng := snapshot.New(layout, gitx.ExecRunner{})
	if err := eng.Layout.EnsureRepoDirs(work); err != nil {
		t.Fatal(err)
	}
	// An old session under the same project, with no live socket -> GC target.
	oldDir := layout.SessionGitDir(work, "ancient")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldDir, "data")
	if err := os.WriteFile(oldFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}

	d := New(eng, layout, "sess1", nil)
	d.IdleTimeout = 100 * time.Millisecond
	ln, err := Listen(layout, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { d.Serve(ln); close(done) }()

	// Register the workdir so exit-GC has something to scan.
	c, err := net.Dial("unix", layout.SocketPath("sess1"))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(c).Encode(Request{Op: OpWarm, Workdir: work, SessionID: "sess1"})
	var resp Response
	_ = json.NewDecoder(c).Decode(&resp)
	c.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		d.Stop()
		t.Fatal("daemon did not idle-exit")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("idle-exit GC should have removed the ancient session, stat err=%v", err)
	}
}

func TestOriginLandsInJournal(t *testing.T) {
	td := start(t)
	td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "rm x", Origin: "codex"})
	td.writeFile(t, "x", "created")
	td.call(t, Request{Op: OpPost, ToolUseID: "t1", Origin: "codex"})
	e := td.journalEntries(t)
	if e["t1"].Origin != "codex" {
		t.Fatalf("origin missing or wrong: %+v", e["t1"])
	}
}

func TestListenRefusesWhenAlive(t *testing.T) {
	td := start(t)
	if _, err := Listen(td.layout, "sess1"); err != ErrAlreadyRunning {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
}

func TestListenClearsStaleSocket(t *testing.T) {
	layout := paths.New(filepath.Join(t.TempDir(), ".bashback"))
	if err := os.MkdirAll(layout.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// A leftover regular file at the socket path (crashed daemon).
	if err := os.WriteFile(layout.SocketPath("dead"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(layout, "dead")
	if err != nil {
		t.Fatalf("Listen should clear stale socket: %v", err)
	}
	ln.Close()
}

// warm reports how many journal entries the requesting session already owns, so
// a resume/compact SessionStart can tell the agent it has prior snapshots.
func TestWarmReportsSessionEntries(t *testing.T) {
	td := start(t)
	if err := td.layout.EnsureRepoDirs(td.work); err != nil {
		t.Fatal(err)
	}
	jpath := td.layout.JournalPath(td.work)
	for _, id := range []string{"a", "b", "c"} {
		if err := journal.Append(jpath, journal.Entry{ToolUseID: id, SessionID: "sess1", PostSHA: "p" + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"x", "y"} {
		if err := journal.Append(jpath, journal.Entry{ToolUseID: id, SessionID: "other", PostSHA: "p" + id}); err != nil {
			t.Fatal(err)
		}
	}
	r := td.call(t, Request{Op: OpWarm, SessionID: "sess1"})
	if !r.OK {
		t.Fatalf("warm failed: %+v", r)
	}
	if r.SessionEntries != 3 {
		t.Fatalf("SessionEntries = %d, want 3", r.SessionEntries)
	}
}

// With no journal yet, warm still succeeds (EnsureRepo + initial pre) and reports
// zero entries — a read failure must not break the prewarm.
func TestWarmNoJournalZero(t *testing.T) {
	td := start(t)
	r := td.call(t, Request{Op: OpWarm, SessionID: "sess1"})
	if !r.OK {
		t.Fatalf("warm failed: %+v", r)
	}
	if r.SessionEntries != 0 {
		t.Fatalf("SessionEntries = %d, want 0", r.SessionEntries)
	}
}

// clock is a deterministic, concurrency-safe time source for daemon tests; the
// daemon reads it via Engine.Now from its worker goroutine.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(rfc3339 string) *clock {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(err)
	}
	return &clock{t: t}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

// A degraded pre (half-row in the journal, no in-memory inflight) must pair with
// a later daemon post instead of being dropped as unprotected.
func TestHandlePostPairsWithDegradedPreHalfRow(t *testing.T) {
	td := start(t)
	if r := td.call(t, Request{Op: OpPre, ToolUseID: "t1", Command: "touch f"}); !r.OK {
		t.Fatalf("pre failed: %+v", r)
	}
	td.writeFile(t, "f", "created by command")
	// Simulate a daemon restart between pre and post: the pre half-row survives in
	// the journal, but the in-memory record is gone.
	td.d.mu.Lock()
	td.d.inflight = map[string]*inflight{}
	td.d.open = map[string]bool{}
	td.d.mu.Unlock()

	r := td.call(t, Request{Op: OpPost, ToolUseID: "t1", Command: "touch f"})
	if !r.OK || r.Status != string(journal.StatusProtected) {
		t.Fatalf("post = %+v, want protected via degraded pre pairing", r)
	}
	e := td.journalEntries(t)["t1"]
	if e.PreSHA == "" || e.PostSHA == "" {
		t.Fatalf("journal entry = %+v, want both pre and post sha", e)
	}
}

func TestListenBindIsMutuallyExclusive(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	layout := paths.New(home)
	for i := 0; i < 20; i++ {
		var wg sync.WaitGroup
		lns := make([]net.Listener, 2)
		errs := make([]error, 2)
		wg.Add(2)
		for j := 0; j < 2; j++ {
			go func(j int) {
				defer wg.Done()
				lns[j], errs[j] = Listen(layout, "sessX")
			}(j)
		}
		wg.Wait()
		wins := 0
		for j := 0; j < 2; j++ {
			if errs[j] == nil {
				wins++
				_ = lns[j].Close()
			}
		}
		if wins != 1 {
			t.Fatalf("iteration %d: %d listeners bound, want exactly 1 (errs=%v)", i, wins, errs)
		}
		_ = os.Remove(layout.SocketPath("sessX"))
	}
}
