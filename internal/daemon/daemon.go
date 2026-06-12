package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// DefaultIdleTimeout bounds how long the daemon waits with no requests before a
// final GC and exit. Idle self-exit is the primary reclamation path, since
// SessionEnd isn't guaranteed to fire.
const DefaultIdleTimeout = 30 * time.Minute

// DefaultStaleTTL bounds how long a pre may stay open with no post before it is
// treated as an orphan (Esc interrupt / permission denial) and swept from the
// open set, so it stops poisoning later commands' overlap flags. Generous — past
// the official 600s bash timeout ceiling.
const DefaultStaleTTL = 15 * time.Minute

// noteInterrupted marks the journal note half-row written when an open pre is
// swept with no post received.
const noteInterrupted = "no post received (interrupted?)"

// workerGitBudget bounds one snapshot's git work inside the daemon. The hook
// client waits at most its own ~5s budget; daemon work past that only burns
// disk for an answer nobody is reading. It is generous (30s) because the daemon
// is a background process that may serve a slow tree; the hook-side wait is cut
// independently by the socket deadline.
const workerGitBudget = 30 * time.Second

// NoteBackground marks an entry whose command ran in the background: the post
// snapshot is taken at the moment of backgrounding, so later writes are not
// protected. Both the daemon and the degraded client write it, so the flag
// survives either path.
const NoteBackground = "background (post taken at backgrounding; later writes unprotected)"

// Daemon serializes snapshot work for one session behind a single worker.
type Daemon struct {
	Engine      *snapshot.Engine
	Layout      paths.Layout
	SessionID   string
	IdleTimeout time.Duration
	StaleTTL    time.Duration
	Logger      *log.Logger

	jobs      chan job
	mu        sync.Mutex
	inflight  map[string]*inflight
	open      map[string]bool // tool_use_ids with pre seen, post pending
	workdirs  map[string]bool // every workdir served, for exit-time GC
	idleReset chan struct{}
	stop      chan struct{}
	stopOnce  sync.Once
}

type inflight struct {
	preSHA     string
	skipped    bool
	command    string
	note       string
	seq        int
	cmdHash    string
	start      time.Time
	overlapped bool
	background bool
	workdir    string // journal path for the orphan-sweep note half-row
	swept      bool   // interrupted note already written; don't repeat
}

type job struct {
	req   Request
	reply chan Response
}

// New builds a Daemon. A nil Logger discards logs.
func New(engine *snapshot.Engine, layout paths.Layout, sessionID string, logger *log.Logger) *Daemon {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Daemon{
		Engine:      engine,
		Layout:      layout,
		SessionID:   sessionID,
		IdleTimeout: DefaultIdleTimeout,
		StaleTTL:    DefaultStaleTTL,
		Logger:      logger,
		jobs:        make(chan job),
		inflight:    map[string]*inflight{},
		open:        map[string]bool{},
		workdirs:    map[string]bool{},
		idleReset:   make(chan struct{}, 1),
		stop:        make(chan struct{}),
	}
}

func (d *Daemon) now() time.Time { return d.Engine.Now() }

// Serve runs the accept loop, the single worker, and the idle timer until the
// daemon is stopped. It owns ln and closes it on return.
func (d *Daemon) Serve(ln net.Listener) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); d.worker() }()
	go func() { defer wg.Done(); d.idleLoop() }()
	go func() { defer wg.Done(); d.sweepLoop() }()

	go d.acceptLoop(ln)

	<-d.stop
	_ = ln.Close()
	wg.Wait()
	d.flushOpen()
	d.runExitGC()
}

func (d *Daemon) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-d.stop:
				return
			default:
				return // listener closed
			}
		}
		go d.handleConn(conn)
	}
}

// handleConn reads requests from one connection and dispatches each to the
// serial worker, writing the response back. Multiple connections are served
// concurrently but their work is serialized by the worker.
func (d *Daemon) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		reply := make(chan Response, 1)
		select {
		case d.jobs <- job{req: req, reply: reply}:
		case <-d.stop:
			_ = enc.Encode(Response{OK: false, Error: "daemon stopping"})
			return
		}
		resp := <-reply
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (d *Daemon) worker() {
	for {
		select {
		case j := <-d.jobs:
			d.touchIdle()
			j.reply <- d.handle(j.req)
		case <-d.stop:
			return
		}
	}
}

func (d *Daemon) handle(req Request) Response {
	switch req.Op {
	case OpPre:
		return d.handlePre(req)
	case OpPost:
		return d.handlePost(req)
	case OpBgFinal:
		return d.handleBgFinal(req)
	case OpWarm:
		return d.handleWarm(req)
	case OpPing:
		return Response{OK: true}
	case OpShutdown:
		d.Stop()
		return Response{OK: true}
	default:
		return Response{OK: false, Error: "unknown op: " + req.Op}
	}
}

func (d *Daemon) handlePre(req Request) Response {
	ctx, cancel := context.WithTimeout(context.Background(), workerGitBudget)
	defer cancel()
	d.noteWorkdir(req.Workdir)
	repo, err := d.Engine.EnsureRepo(ctx, req.Workdir, req.SessionID)
	if err != nil {
		d.Logger.Printf("pre EnsureRepo %s: %v", req.ToolUseID, err)
		return Response{OK: false, Status: string(journal.StatusUnprotectedError), Error: err.Error()}
	}
	pre, err := d.Engine.Pre(ctx, repo, d.forceInclude(req.Workdir))
	if err != nil {
		d.Logger.Printf("pre snapshot %s: %v", req.ToolUseID, err)
		return Response{OK: false, Status: string(journal.StatusUnprotectedError), Error: err.Error()}
	}
	rec := &inflight{
		preSHA:     pre.PreSHA,
		skipped:    pre.Skipped,
		command:    req.Command,
		note:       pre.Note,
		seq:        req.Seq,
		cmdHash:    req.CmdHash,
		start:      d.now(),
		background: req.Background,
		workdir:    req.Workdir,
	}
	d.mu.Lock()
	// Overlap: any other command still open when this pre arrives means their
	// time intervals overlap — mark both.
	if len(d.open) > 0 {
		rec.overlapped = true
		for id := range d.open {
			if other := d.inflight[id]; other != nil {
				other.overlapped = true
			}
		}
	}
	d.inflight[req.ToolUseID] = rec
	d.open[req.ToolUseID] = true
	// Capture under the lock: a later concurrent pre may flip rec.overlapped, so
	// read it here, not outside. The pre half-row records the value at pre time;
	// the post full row carries the authoritative final flag.
	overlapped := rec.overlapped
	d.mu.Unlock()

	// Land a pre half-row immediately so the pre snapshot SHA survives an Esc
	// interrupt or a daemon crash before post. post lands the full row
	// and ReadMerged folds them by key.
	d.appendJournal(req.Workdir, journal.Entry{
		ToolUseID:  req.ToolUseID,
		SessionID:  req.SessionID,
		TS:         rec.start.UTC().Format(time.RFC3339),
		Command:    journal.RedactCommand(req.Command),
		PreSHA:     pre.PreSHA,
		Seq:        req.Seq,
		CmdHash:    req.CmdHash,
		Overlapped: overlapped,
		Note:       strings.TrimSpace(pre.Note),
		Origin:     req.Origin,
	})

	return Response{OK: true, PreSHA: pre.PreSHA, Overlapped: overlapped}
}

func (d *Daemon) handlePost(req Request) Response {
	ctx, cancel := context.WithTimeout(context.Background(), workerGitBudget)
	defer cancel()
	d.noteWorkdir(req.Workdir)

	d.mu.Lock()
	rec := d.inflight[req.ToolUseID]
	delete(d.inflight, req.ToolUseID)
	delete(d.open, req.ToolUseID)
	d.mu.Unlock()

	// run_in_background may be reported on either hook event; honor both the pre
	// record and the post payload.
	background := req.Background || (rec != nil && rec.background)
	appendEntry := func(e journal.Entry) {
		if background {
			e.Note = joinNotes(e.Note, NoteBackground)
		}
		d.appendJournal(req.Workdir, e)
	}

	repo, err := d.Engine.EnsureRepo(ctx, req.Workdir, req.SessionID)
	if err != nil {
		d.Logger.Printf("post EnsureRepo %s: %v", req.ToolUseID, err)
		return Response{OK: false, Status: string(journal.StatusUnprotectedError), Error: err.Error()}
	}

	entry := journal.Entry{
		ToolUseID: req.ToolUseID,
		SessionID: req.SessionID,
		TS:        d.now().UTC().Format(time.RFC3339),
		BgTaskID:  req.BgTaskID,
		Origin:    req.Origin,
	}

	if rec == nil {
		// No in-memory pre for this key. It may have been written degraded while
		// this daemon was unreachable: look for a pre half-row in the journal and
		// pair with it before giving up. Only a truly absent pre is unprotected.
		if pre := journal.PreSHAFor(d.Layout.JournalPath(req.Workdir), journal.DefaultKeyer, req.ToolUseID); pre != "" {
			rec = &inflight{preSHA: pre, command: req.Command, seq: req.Seq, cmdHash: req.CmdHash, start: d.now(), workdir: req.Workdir}
		} else {
			// Pre was lost (daemon restarted mid-command). We can still snapshot the
			// post state but have no pre baseline, so it is unprotected.
			entry.Command = journal.RedactCommand(req.Command)
			entry.Seq, entry.CmdHash = req.Seq, req.CmdHash
			entry.Status = journal.StatusUnprotectedError
			entry.Note = "pre snapshot missing (daemon restart?)"
			appendEntry(entry)
			return Response{OK: false, Status: string(entry.Status)}
		}
	}

	post, perr := d.Engine.Post(ctx, repo, snapshot.PreResult{PreSHA: rec.preSHA, Skipped: rec.skipped}, d.forceInclude(req.Workdir))
	entry.Command = journal.RedactCommand(rec.command)
	entry.PreSHA = rec.preSHA
	entry.Seq, entry.CmdHash = rec.seq, rec.cmdHash
	entry.Overlapped = rec.overlapped
	entry.DurationMS = d.now().Sub(rec.start).Milliseconds()
	if perr != nil {
		d.Logger.Printf("post snapshot %s: %v", req.ToolUseID, perr)
		entry.Status = journal.StatusUnprotectedError
		entry.Note = strings.TrimSpace(rec.note)
		appendEntry(entry)
		return Response{OK: false, Status: string(entry.Status), Error: perr.Error()}
	}
	entry.PostSHA = post.PostSHA
	entry.Status = post.Status
	entry.Note = strings.TrimSpace(joinNotes(rec.note, post.Note))
	entry.Files = post.Files
	entry.FilesOmitted = post.FilesOmitted
	appendEntry(entry)

	deletes := 0
	for _, f := range post.Files {
		if f.S == "D" {
			deletes++
		}
	}
	return Response{
		OK: true, Status: string(entry.Status), PreSHA: entry.PreSHA, PostSHA: entry.PostSHA, Overlapped: entry.Overlapped,
		Files: len(post.Files), Deletes: deletes, Key: journal.DefaultKeyer.Key(entry),
	}
}

// handleBgFinal captures a backgrounded command's final on-disk state at the
// moment its TaskOutput-completed / TaskStop event fires. It runs on
// the serial worker, so the journal read/append is race-free like the other ops.
func (d *Daemon) handleBgFinal(req Request) Response {
	ctx, cancel := context.WithTimeout(context.Background(), workerGitBudget)
	defer cancel()
	d.noteWorkdir(req.Workdir)
	res, err := d.Engine.BgFinal(ctx, req.Workdir, req.SessionID, req.BgTaskID, d.forceInclude(req.Workdir))
	if err != nil {
		d.Logger.Printf("bgfinal %s: %v", req.BgTaskID, err)
		return Response{OK: false, Error: err.Error()}
	}
	return Response{OK: true, Files: res.Files, Deletes: res.Deletes, Key: res.Key}
}

// handleWarm prewarms the session: ensure the repo and take the initial cold
// baseline snapshot so the first real bash is fast.
func (d *Daemon) handleWarm(req Request) Response {
	ctx := context.Background()
	d.noteWorkdir(req.Workdir)
	repo, err := d.Engine.EnsureRepo(ctx, req.Workdir, req.SessionID)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if _, err := d.Engine.Pre(ctx, repo, d.forceInclude(req.Workdir)); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	return Response{OK: true, SessionEntries: d.sessionEntryCount(req.Workdir, req.SessionID)}
}

// sessionEntryCount returns how many merged journal entries belong to the given
// session. A read failure counts as zero — warm never fails on this.
func (d *Daemon) sessionEntryCount(workdir, sessionID string) int {
	entries, err := journal.ReadMerged(d.Layout.JournalPath(workdir), journal.DefaultKeyer)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.SessionID == sessionID {
			n++
		}
	}
	return n
}

func (d *Daemon) appendJournal(workdir string, e journal.Entry) {
	if err := journal.Append(d.Layout.JournalPath(workdir), e); err != nil {
		d.Logger.Printf("journal append: %v", err)
	}
}

func (d *Daemon) forceInclude(workdir string) []string {
	m, err := d.Layout.ReadMeta(workdir)
	if err != nil {
		return nil
	}
	return m.ForceInclude
}

func (d *Daemon) noteWorkdir(workdir string) {
	if workdir == "" {
		return
	}
	d.mu.Lock()
	d.workdirs[workdir] = true
	d.mu.Unlock()
}

// Stop signals the daemon to shut down. Safe to call multiple times.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

func (d *Daemon) staleTTL() time.Duration {
	if d.StaleTTL > 0 {
		return d.StaleTTL
	}
	return DefaultStaleTTL
}

type orphanNote struct {
	workdir string
	entry   journal.Entry
}

// collectOrphans removes open pres that have no post and returns the note
// half-rows to append. When all is false only those past the stale TTL are
// swept; when true (exit flush) every still-open pre is swept. The inflight
// record is kept so a late post can still complete the entry — only the open
// flag is cleared so the orphan stops marking later commands overlapped.
func (d *Daemon) collectOrphans(all bool) []orphanNote {
	ttl := d.staleTTL()
	now := d.now()
	var notes []orphanNote
	d.mu.Lock()
	for id := range d.open {
		rec := d.inflight[id]
		if rec == nil {
			delete(d.open, id)
			continue
		}
		if !all && now.Sub(rec.start) <= ttl {
			continue
		}
		delete(d.open, id)
		if rec.swept {
			continue
		}
		rec.swept = true
		// Leave SessionID empty so the merge keeps the pre half-row's value.
		notes = append(notes, orphanNote{
			workdir: rec.workdir,
			entry:   journal.Entry{ToolUseID: id, Note: noteInterrupted},
		})
	}
	d.mu.Unlock()
	return notes
}

func (d *Daemon) writeOrphans(notes []orphanNote) {
	for _, n := range notes {
		if n.workdir == "" {
			continue
		}
		d.appendJournal(n.workdir, n.entry)
	}
}

// sweepOpen is the TTL pass: orphan any open pre older than the stale TTL.
func (d *Daemon) sweepOpen() { d.writeOrphans(d.collectOrphans(false)) }

// flushOpen orphans every still-open pre, run once on shutdown.
func (d *Daemon) flushOpen() { d.writeOrphans(d.collectOrphans(true)) }

// sweepLoop periodically runs the TTL pass until the daemon stops. It is a
// safety net; deterministic tests drive sweepOpen directly with a fake clock.
func (d *Daemon) sweepLoop() {
	interval := d.staleTTL()
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.sweepOpen()
		case <-d.stop:
			return
		}
	}
}

func (d *Daemon) touchIdle() {
	select {
	case d.idleReset <- struct{}{}:
	default:
	}
}

func (d *Daemon) idleLoop() {
	timeout := d.IdleTimeout
	if timeout <= 0 {
		timeout = DefaultIdleTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-d.idleReset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			d.Logger.Printf("idle %s: shutting down", timeout)
			d.Stop()
			return
		case <-d.stop:
			return
		}
	}
}

// runExitGC runs one GC pass before exit, preserving sessions whose sockets are
// still live.
func (d *Daemon) runExitGC() {
	active := d.liveSessions()
	d.mu.Lock()
	workdirs := make([]string, 0, len(d.workdirs))
	for w := range d.workdirs {
		workdirs = append(workdirs, w)
	}
	d.mu.Unlock()
	for _, w := range workdirs {
		if _, err := d.Engine.GC(w, snapshot.GCOpts{ActiveSessions: active}); err != nil {
			d.Logger.Printf("exit gc %s: %v", w, err)
		}
	}
}

// liveSessions scans run/ for live sockets (other daemons), excluding our own
// which is about to close.
func (d *Daemon) liveSessions() map[string]bool {
	active := map[string]bool{}
	ents, err := os.ReadDir(d.Layout.RunDir())
	if err != nil {
		return active
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		sid := strings.TrimSuffix(name, ".sock")
		if sid == d.SessionID {
			continue
		}
		if socketAlive(filepath.Join(d.Layout.RunDir(), name)) {
			active[sid] = true
		}
	}
	return active
}

func joinNotes(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// socketAlive reports whether a unix socket currently has a listener.
func socketAlive(path string) bool {
	c, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
