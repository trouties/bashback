package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/daemon"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/lockfile"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

func newClient(t *testing.T) (*Client, paths.Layout, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	layout := paths.New(home)
	if err := layout.EnsureRepoDirs(work); err != nil {
		t.Fatal(err)
	}
	eng := snapshot.New(layout, gitx.ExecRunner{})
	c := New(layout, eng, "sess1")
	c.DialTimeout = 100 * time.Millisecond
	c.SpawnWait = 300 * time.Millisecond
	return c, layout, work
}

// stubDaemon listens on the session socket and replies with a canned response,
// recording the request it saw.
func stubDaemon(t *testing.T, layout paths.Layout, sessionID string, resp daemon.Response) *daemon.Request {
	t.Helper()
	if err := os.MkdirAll(layout.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", layout.SocketPath(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	got := &daemon.Request{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = json.NewDecoder(conn).Decode(got)
		_ = json.NewEncoder(conn).Encode(resp)
	}()
	t.Cleanup(func() { ln.Close() })
	return got
}

func TestSnapshotUsesSocketWhenAvailable(t *testing.T) {
	c, layout, work := newClient(t)
	want := daemon.Response{OK: true, Status: string(journal.StatusProtected), PreSHA: "abc"}
	got := stubDaemon(t, layout, "sess1", want)

	resp := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1"})
	if !resp.OK || resp.PreSHA != "abc" {
		t.Fatalf("socket response not used: %+v", resp)
	}
	if got.ToolUseID != "t1" {
		t.Fatalf("daemon saw wrong request: %+v", got)
	}
}

func TestSnapshotSpawnsThenUsesSocket(t *testing.T) {
	c, layout, work := newClient(t)
	want := daemon.Response{OK: true, PreSHA: "spawned"}
	// Spawn lazily brings up the stub daemon.
	c.Spawn = func() error {
		stubDaemon(t, layout, "sess1", want)
		return nil
	}
	resp := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1"})
	if !resp.OK || resp.PreSHA != "spawned" {
		t.Fatalf("did not use spawned daemon: %+v", resp)
	}
}

func TestSnapshotDegradedDirectWrite(t *testing.T) {
	c, layout, work := newClient(t)
	c.Spawn = func() error { return errors.New("daemon unavailable") } // skip spawn->socket

	// Degraded pre writes a half-row.
	pre := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Command: "touch f"})
	if !pre.OK || pre.PreSHA == "" {
		t.Fatalf("degraded pre failed: %+v", pre)
	}
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Degraded post writes the sibling half-row.
	post := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPost, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Command: "touch f"})
	if !post.OK || post.Status != string(journal.StatusProtected) {
		t.Fatalf("degraded post = %+v, want protected", post)
	}

	merged, err := journal.ReadMerged(layout.JournalPath(work), journal.ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 {
		t.Fatalf("half-rows should merge to 1 entry, got %d", len(merged))
	}
	e := merged[0]
	if e.PreSHA == "" || e.PostSHA == "" || e.Status != journal.StatusProtected {
		t.Fatalf("merged degraded entry incomplete: %+v", e)
	}
}

// In the degraded path (no daemon), warm is still a snapshot no-op but now counts
// the session's prior journal entries so the resume/compact hint is accurate.
func TestDegradedWarmCountsEntries(t *testing.T) {
	c, layout, work := newClient(t)
	c.Spawn = func() error { return errors.New("daemon unavailable") } // force degraded

	jpath := layout.JournalPath(work)
	for _, id := range []string{"a", "b"} {
		if err := journal.Append(jpath, journal.Entry{ToolUseID: id, SessionID: "sess1", PostSHA: "p" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Append(jpath, journal.Entry{ToolUseID: "z", SessionID: "other", PostSHA: "pz"}); err != nil {
		t.Fatal(err)
	}

	resp := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpWarm, Workdir: work, SessionID: "sess1"})
	if !resp.OK || resp.SessionEntries != 2 {
		t.Fatalf("degraded warm SessionEntries = %d (ok=%v), want 2", resp.SessionEntries, resp.OK)
	}
}

func TestSnapshotGivesUpGracefully(t *testing.T) {
	c, layout, _ := newClient(t)
	c.Engine = nil // no degraded path available
	c.Spawn = func() error { return errors.New("nope") }
	_ = layout

	resp := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPre, Workdir: "/x", SessionID: "sess1", ToolUseID: "t1"})
	if resp.OK {
		t.Fatalf("expected graceful give-up, got OK: %+v", resp)
	}
	if resp.Status != string(journal.StatusUnprotectedError) {
		t.Fatalf("status = %q, want unprotected_error", resp.Status)
	}
}

// degradedClient forces the direct-write path (no daemon) and pins the engine
// clock so degraded entries get deterministic timestamps.
func degradedClient(t *testing.T) (*Client, paths.Layout, string, *time.Time) {
	t.Helper()
	c, layout, work := newClient(t)
	c.Spawn = func() error { return errors.New("daemon unavailable") }
	clock := time.Now().UTC()
	now := &clock
	c.Engine.Now = func() time.Time { return *now }
	return c, layout, work, now
}

func degradedPre(t *testing.T, c *Client, work, session, id, cmd string) {
	t.Helper()
	resp := c.Snapshot(context.Background(), daemon.Request{
		Op: daemon.OpPre, Workdir: work, SessionID: session, ToolUseID: id, Command: cmd,
	})
	if !resp.OK {
		t.Fatalf("degraded pre %q failed: %+v", id, resp)
	}
}

func overlapByID(t *testing.T, layout paths.Layout, work string) map[string]bool {
	t.Helper()
	merged, err := journal.ReadMerged(layout.JournalPath(work), journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range merged {
		out[e.ToolUseID] = e.Overlapped
	}
	return out
}

// TestDegradedPreMarksOverlapBothWays: two degraded pres (no posts) → both the
// new pre and the open pre read overlapped, mirroring the daemon's pre-time
// two-way marking.
func TestDegradedPreMarksOverlapBothWays(t *testing.T) {
	c, layout, work, _ := degradedClient(t)
	degradedPre(t, c, work, "sess1", "a", "touch a")
	degradedPre(t, c, work, "sess1", "b", "touch b")

	ov := overlapByID(t, layout, work)
	if !ov["a"] || !ov["b"] {
		t.Fatalf("both pres should be overlapped, got %+v", ov)
	}
}

// TestDegradedPreNoOpenPreNoFlag: when the prior command already has its post
// half-row, it is not open — a new pre is not flagged and the closed entry is
// not touched.
func TestDegradedPreNoOpenPreNoFlag(t *testing.T) {
	c, layout, work, _ := degradedClient(t)
	degradedPre(t, c, work, "sess1", "a", "touch a")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPost, Workdir: work, SessionID: "sess1", ToolUseID: "a", Command: "touch a"})
	if !post.OK {
		t.Fatalf("degraded post failed: %+v", post)
	}
	degradedPre(t, c, work, "sess1", "b", "touch b")

	ov := overlapByID(t, layout, work)
	if ov["a"] || ov["b"] {
		t.Fatalf("closed prior pre must not overlap, got %+v", ov)
	}
}

// TestDegradedPreIgnoresStaleOpenPre: an open pre older than the stale TTL is not
// counted, so a much-later pre is not flagged.
func TestDegradedPreIgnoresStaleOpenPre(t *testing.T) {
	c, layout, work, now := degradedClient(t)
	degradedPre(t, c, work, "sess1", "a", "touch a")
	*now = now.Add(config.DefaultStaleTTL + time.Minute)
	degradedPre(t, c, work, "sess1", "b", "touch b")

	ov := overlapByID(t, layout, work)
	if ov["a"] || ov["b"] {
		t.Fatalf("stale open pre must not trigger overlap, got %+v", ov)
	}
}

func TestDegradedOriginLandsInJournal(t *testing.T) {
	c, layout, work, _ := degradedClient(t)
	pre := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Command: "rm x", Origin: "codex"})
	if !pre.OK {
		t.Fatalf("degraded pre failed: %+v", pre)
	}
	if err := os.WriteFile(filepath.Join(work, "x"), []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpPost, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Origin: "codex"})
	if !post.OK {
		t.Fatalf("degraded post failed: %+v", post)
	}
	merged, err := journal.ReadMerged(layout.JournalPath(work), journal.DefaultKeyer)
	if err != nil || len(merged) == 0 || merged[len(merged)-1].Origin != "codex" {
		t.Fatalf("origin missing: %v err %v", merged, err)
	}
}

// TestDegradedPreIgnoresManual: a pre-only manual snapshot is not an open command
// pre and does not participate in overlap judgement.
func TestDegradedPreIgnoresManual(t *testing.T) {
	c, layout, work, now := degradedClient(t)
	if err := journal.Append(layout.JournalPath(work), journal.Entry{
		ToolUseID: "snap1", SessionID: "sess1", TS: now.Format(time.RFC3339),
		PreSHA: "deadbeef", Status: journal.StatusManual,
	}); err != nil {
		t.Fatal(err)
	}
	degradedPre(t, c, work, "sess1", "b", "touch b")

	ov := overlapByID(t, layout, work)
	if ov["snap1"] || ov["b"] {
		t.Fatalf("manual entry must not trigger overlap, got %+v", ov)
	}
}

// silentSocket binds the session socket and accepts connections but never
// replies, so a client that fails to bound its read would hang forever.
func silentSocket(t *testing.T, layout paths.Layout, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(layout.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", layout.SocketPath(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and then sit silent; never write a response.
			_ = conn
		}
	}()
	t.Cleanup(func() { ln.Close() })
}

func TestTrySocketTimesOutOnSilentServer(t *testing.T) {
	t.Setenv("BASHBACK_NO_SPAWN", "1")
	c, layout, work := newClient(t)
	silentSocket(t, layout, "sess1")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.Snapshot(ctx, daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Command: "touch f"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Snapshot hung on a silent socket server")
	}
}

func TestDegradedLockTimeoutIsJournaled(t *testing.T) {
	c, layout, work, now := degradedClient(t)
	lock, err := lockfile.Acquire(c.lockPath(work), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp := c.Snapshot(ctx, daemon.Request{Op: daemon.OpPre, Workdir: work, SessionID: "sess1", ToolUseID: "t1", Command: "rm x"})
	if resp.Status != string(journal.StatusUnprotectedTimeout) {
		t.Fatalf("status = %q, want unprotected_timeout", resp.Status)
	}
	_ = now
	entries, err := journal.ReadMerged(layout.JournalPath(work), journal.DefaultKeyer)
	if err != nil {
		t.Fatal(err)
	}
	var found *journal.Entry
	for i := range entries {
		if entries[i].ToolUseID == "t1" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("no journal row recorded for the timed-out command")
	}
	if found.Status != journal.StatusUnprotectedTimeout || found.Note == "" {
		t.Fatalf("journal entry = %+v, want unprotected_timeout with a note", *found)
	}
}

func TestShutdownWithoutDaemonIsCheapNoop(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	layout := paths.New(home)
	eng := snapshot.New(layout, gitx.ExecRunner{})
	c := New(layout, eng, "sess1")
	c.DialTimeout = 100 * time.Millisecond
	spawned := false
	c.Spawn = func() error { spawned = true; return nil }

	resp := c.Snapshot(context.Background(), daemon.Request{Op: daemon.OpShutdown, Workdir: work, SessionID: "sess1"})
	if !resp.OK {
		t.Fatalf("shutdown of an absent daemon should be a no-op OK, got %+v", resp)
	}
	if spawned {
		t.Error("shutdown must never spawn a daemon")
	}
	if _, err := os.Stat(layout.RepoDir(work)); !os.IsNotExist(err) {
		t.Error("shutdown created repo dirs for a goodbye")
	}
}
