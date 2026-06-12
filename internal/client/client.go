// Package client implements the hook-side degradation chain: try the
// session socket, lazily spawn the daemon, fall back to a flock-serialized
// direct write, and finally give up gracefully. It never blocks the agent —
// every path returns a Response the hook turns into exit 0.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/daemon"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/lockfile"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

var errSpawnDisabled = errors.New("daemon spawn disabled via BASHBACK_NO_SPAWN")

// Client drives one snapshot request through the degradation chain.
type Client struct {
	Layout    paths.Layout
	Engine    *snapshot.Engine
	SessionID string
	// Spawn lazily starts the daemon. Returns nil if a daemon should now be
	// reachable. Injectable for tests; New sets a process-spawning default.
	Spawn func() error
	// DialTimeout bounds a single socket dial.
	DialTimeout time.Duration
	// SpawnWait bounds how long to wait for a freshly spawned daemon to bind.
	SpawnWait time.Duration
}

// New builds a Client with a default daemon-spawning implementation.
func New(layout paths.Layout, engine *snapshot.Engine, sessionID string) *Client {
	c := &Client{
		Layout:      layout,
		Engine:      engine,
		SessionID:   sessionID,
		DialTimeout: 500 * time.Millisecond,
		SpawnWait:   2 * time.Second,
	}
	c.Spawn = func() error { return spawnDaemon(layout, sessionID) }
	return c
}

// Snapshot runs the degradation chain for one request and always returns a
// Response (never an error): socket → spawn+socket → degraded flock → give up.
func (c *Client) Snapshot(ctx context.Context, req daemon.Request) daemon.Response {
	if req.Op == daemon.OpShutdown {
		// A goodbye to a daemon that isn't there is a no-op; never spawn a daemon
		// (or create repo dirs) just to tell it to shut down.
		if resp, ok := c.trySocket(ctx, req); ok {
			return resp
		}
		return daemon.Response{OK: true}
	}
	if resp, ok := c.trySocket(ctx, req); ok {
		return resp
	}
	if c.Spawn != nil {
		if err := c.Spawn(); err == nil {
			if resp, ok := c.waitSocket(ctx, req); ok {
				return resp
			}
		}
	}
	if c.Engine != nil {
		if resp, ok := c.degraded(ctx, req); ok {
			return resp
		}
	}
	// Last resort: let the command through unprotected.
	return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedError)}
}

func (c *Client) socketPath() string { return c.Layout.SocketPath(c.SessionID) }

func (c *Client) trySocket(ctx context.Context, req daemon.Request) (daemon.Response, bool) {
	conn, err := net.DialTimeout("unix", c.socketPath(), c.DialTimeout)
	if err != nil {
		return daemon.Response{}, false
	}
	defer func() { _ = conn.Close() }()
	// A daemon that accepts but never replies must not hang the hook: bound the
	// whole exchange by the caller's deadline, or a 2s default when none is set.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return daemon.Response{}, false
	}
	var resp daemon.Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return daemon.Response{}, false
	}
	return resp, true
}

// waitSocket retries dialing a just-spawned daemon until it binds or the budget
// (ctx deadline / SpawnWait) runs out.
func (c *Client) waitSocket(ctx context.Context, req daemon.Request) (daemon.Response, bool) {
	deadline := time.Now().Add(c.SpawnWait)
	for {
		if resp, ok := c.trySocket(ctx, req); ok {
			return resp, true
		}
		if time.Now().After(deadline) || ctxDone(ctx) {
			return daemon.Response{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// degraded performs the snapshot directly under a flock on the shadow repo,
// writing a half-row to the journal (merged with its sibling on read). It is the
// fallback when no daemon is reachable.
func (c *Client) degraded(ctx context.Context, req daemon.Request) (daemon.Response, bool) {
	budget := remaining(ctx, time.Second)
	lock, err := lockfile.Acquire(c.lockPath(req.Workdir), budget)
	if err != nil {
		// A pre/post that never ran for want of the lock would otherwise vanish
		// from the ledger; record an explicit unprotected row so the gap is
		// visible to list/doctor and a later restore has an honest answer.
		if req.Op == daemon.OpPre || req.Op == daemon.OpPost {
			if derr := c.Layout.EnsureRepoDirs(req.Workdir); derr == nil {
				_ = journal.Append(c.Layout.JournalPath(req.Workdir), journal.Entry{
					ToolUseID: req.ToolUseID, SessionID: req.SessionID,
					TS:     c.Engine.Now().UTC().Format(time.RFC3339),
					Status: journal.StatusUnprotectedTimeout,
					Note:   "lock wait exceeded the hook budget; this command is unprotected",
					Seq:    req.Seq, CmdHash: req.CmdHash, Origin: req.Origin,
				})
			}
		}
		return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedTimeout)}, true
	}
	defer func() { _ = lock.Release() }()

	repo, err := c.Engine.EnsureRepo(ctx, req.Workdir, req.SessionID)
	if err != nil {
		return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedError)}, true
	}
	jpath := c.Layout.JournalPath(req.Workdir)
	ts := c.Engine.Now().UTC().Format(time.RFC3339)

	switch req.Op {
	case daemon.OpPre:
		pre, err := c.Engine.Pre(ctx, repo, c.forceInclude(req.Workdir))
		if err != nil {
			return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedError)}, true
		}
		overlapped := c.markDegradedOverlap(jpath, req.Workdir, req.ToolUseID, c.Engine.Now())
		_ = journal.Append(jpath, journal.Entry{
			ToolUseID: req.ToolUseID, SessionID: req.SessionID, TS: ts,
			Command: journal.RedactCommand(req.Command), PreSHA: pre.PreSHA,
			Seq: req.Seq, CmdHash: req.CmdHash, Note: pre.Note,
			Overlapped: overlapped, Origin: req.Origin,
		})
		return daemon.Response{OK: true, PreSHA: pre.PreSHA}, true

	case daemon.OpPost:
		preSHA := c.recoverPreSHA(jpath, req.ToolUseID)
		post, err := c.Engine.Post(ctx, repo, snapshot.PreResult{PreSHA: preSHA}, c.forceInclude(req.Workdir))
		if err != nil {
			_ = journal.Append(jpath, journal.Entry{
				ToolUseID: req.ToolUseID, SessionID: req.SessionID, TS: ts,
				Status: journal.StatusUnprotectedError, Seq: req.Seq, CmdHash: req.CmdHash,
			})
			return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedError)}, true
		}
		note := post.Note
		if req.Background {
			note = joinNote(note, daemon.NoteBackground)
		}
		entry := journal.Entry{
			ToolUseID: req.ToolUseID, SessionID: req.SessionID, TS: ts,
			PostSHA: post.PostSHA, Status: post.Status, Seq: req.Seq, CmdHash: req.CmdHash, Note: note,
			Files: post.Files, FilesOmitted: post.FilesOmitted, BgTaskID: req.BgTaskID, Origin: req.Origin,
		}
		_ = journal.Append(jpath, entry)
		deletes := 0
		for _, f := range post.Files {
			if f.S == "D" {
				deletes++
			}
		}
		return daemon.Response{
			OK: true, Status: string(post.Status), PreSHA: preSHA, PostSHA: post.PostSHA,
			Files: len(post.Files), Deletes: deletes, Key: journal.DefaultKeyer.Key(entry),
		}, true

	case daemon.OpBgFinal:
		res, err := c.Engine.BgFinal(ctx, req.Workdir, req.SessionID, req.BgTaskID, c.forceInclude(req.Workdir))
		if err != nil {
			return daemon.Response{OK: false, Status: string(journal.StatusUnprotectedError)}, true
		}
		return daemon.Response{OK: true, Files: res.Files, Deletes: res.Deletes, Key: res.Key}, true

	case daemon.OpWarm:
		// No daemon to prewarm; still count this session's prior entries so a
		// resume/compact SessionStart hint is accurate. No snapshot is captured.
		n := 0
		entries, err := journal.ReadMerged(jpath, journal.DefaultKeyer)
		if err == nil {
			for _, e := range entries {
				if e.SessionID == req.SessionID {
					n++
				}
			}
		}
		return daemon.Response{OK: true, SessionEntries: n}, true

	default:
		// ping/shutdown have no degraded meaning; treat as no-op success.
		return daemon.Response{OK: true}, true
	}
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// markDegradedOverlap mirrors the daemon's pre-time two-way overlap marking onto
// the degraded direct-write path, where only the journal is shared. An open pre
// is a pre-only half-row, not manual, from a different command, younger than the
// stale TTL. When any exist, this pre is overlapped and each open pre gets an
// additive {ToolUseID, Overlapped} half-row so both sides read overlapped after
// merge. Read failures fail open (skip marking, still snapshot).
func (c *Client) markDegradedOverlap(jpath, workdir, toolUseID string, now time.Time) bool {
	entries, err := journal.ReadMerged(jpath, journal.DefaultKeyer)
	if err != nil {
		return false
	}
	ttl := config.Load(c.Layout, workdir, config.OSEnv()).StaleTTL
	var open []journal.Entry
	for _, e := range entries {
		if e.PreSHA == "" || e.PostSHA != "" || e.Status == journal.StatusManual {
			continue
		}
		if e.ToolUseID == toolUseID {
			continue
		}
		t, perr := time.Parse(time.RFC3339, e.TS)
		if perr != nil || now.Sub(t) >= ttl {
			continue
		}
		open = append(open, e)
	}
	if len(open) == 0 {
		return false
	}
	for _, e := range open {
		_ = journal.Append(jpath, journal.Entry{ToolUseID: e.ToolUseID, Overlapped: true})
	}
	return true
}

func (c *Client) recoverPreSHA(jpath, toolUseID string) string {
	return journal.PreSHAFor(jpath, journal.DefaultKeyer, toolUseID)
}

func (c *Client) lockPath(workdir string) string {
	return filepath.Join(c.Layout.RepoDir(workdir), "lock")
}

func (c *Client) forceInclude(workdir string) []string {
	m, err := c.Layout.ReadMeta(workdir)
	if err != nil {
		return nil
	}
	return m.ForceInclude
}

// spawnDaemon starts a detached `bashback daemon run` for the session. Setting
// BASHBACK_NO_SPAWN disables it (escape hatch; also keeps tests from forking the
// test binary), forcing the degraded direct-write path.
func spawnDaemon(layout paths.Layout, sessionID string) error {
	if os.Getenv("BASHBACK_NO_SPAWN") != "" {
		return errSpawnDisabled
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "run", "--session", sessionID)
	cmd.Env = append(os.Environ(), "BASHBACK_HOME="+layout.Root)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Don't reap; the daemon outlives this client.
	_ = cmd.Process.Release()
	return nil
}

func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func remaining(ctx context.Context, fallback time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
		return 0
	}
	return fallback
}
