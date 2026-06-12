package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/paths"
)

// recentWriteGrace shields sessions with fresh writes from the soft-cap pass:
// "active" (live socket) misses degraded-mode and manual sessions, so recency of
// the last write is the honest liveness signal.
const recentWriteGrace = time.Hour

// GCOpts controls reclamation. Active sessions (live socket) are never touched;
// the journal is never deleted.
type GCOpts struct {
	OlderThan      time.Duration
	SoftCapBytes   int64
	DryRun         bool
	ActiveSessions map[string]bool
}

// GCReport summarizes what GC removed (or would remove under DryRun).
type GCReport struct {
	Removed    []string // session ids
	Kept       []string // session ids
	FreedBytes int64
	DryRun     bool
}

type sessionInfo struct {
	id     string
	dir    string
	size   int64
	newest time.Time
	active bool
}

// GC reclaims expired and over-cap session repos for one project. Expiry is
// driven by the last write time; the soft cap evicts oldest-first. Active
// sessions and the journal are preserved.
func (e *Engine) GC(workdir string, opts GCOpts) (GCReport, error) {
	return e.gcRepo(e.Layout.RepoDir(workdir), opts)
}

// ProjectGCReport pairs a project with its reclamation outcome for `gc --all`.
// Project is the original project path when meta.json records it, else the repo
// hash.
type ProjectGCReport struct {
	Project string
	Hash    string
	Report  GCReport
}

// GCAll runs GC across every project repo under the layout root, each
// independently under the same options. A project whose repo can't be read is
// skipped rather than failing the whole sweep; active sessions and journals are
// preserved exactly as in single-project GC.
func (e *Engine) GCAll(opts GCOpts) ([]ProjectGCReport, error) {
	reposRoot := filepath.Join(e.Layout.Root, "repos")
	ents, err := os.ReadDir(reposRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reports []ProjectGCReport
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		repoDir := filepath.Join(reposRoot, ent.Name())
		rep, gerr := e.gcRepo(repoDir, opts)
		if gerr != nil {
			continue
		}
		reports = append(reports, ProjectGCReport{
			Project: projectLabel(repoDir, ent.Name()),
			Hash:    ent.Name(),
			Report:  rep,
		})
	}
	return reports, nil
}

// projectLabel returns the human-readable original path for a repo dir, falling
// back to the hash when meta.json is absent or unparseable.
func projectLabel(repoDir, hash string) string {
	b, err := os.ReadFile(filepath.Join(repoDir, "meta.json"))
	if err != nil {
		return hash
	}
	var m paths.Meta
	if json.Unmarshal(b, &m) == nil && m.OriginalPath != "" {
		return m.OriginalPath
	}
	return hash
}

// gcRepo reclaims one project's session repos given its repo dir directly, so
// both single-project GC and the cross-project sweep share one implementation.
func (e *Engine) gcRepo(repoDir string, opts GCOpts) (GCReport, error) {
	if opts.OlderThan <= 0 {
		opts.OlderThan = DefaultRetention
	}
	if opts.SoftCapBytes <= 0 {
		opts.SoftCapBytes = DefaultSoftCapBytes
	}
	rep := GCReport{DryRun: opts.DryRun}

	sessionsDir := filepath.Join(repoDir, "sessions")
	ents, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, err
	}

	var sessions []sessionInfo
	for _, ent := range ents {
		if !ent.IsDir() || !strings.HasSuffix(ent.Name(), ".git") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".git")
		dir := filepath.Join(sessionsDir, ent.Name())
		size, newest := dirStats(dir)
		sessions = append(sessions, sessionInfo{
			id:     id,
			dir:    dir,
			size:   size,
			newest: newest,
			active: opts.ActiveSessions[id],
		})
	}

	cutoff := e.now().Add(-opts.OlderThan)
	remove := func(s sessionInfo) {
		if !opts.DryRun {
			_ = os.RemoveAll(s.dir)
		}
		rep.Removed = append(rep.Removed, s.id)
		rep.FreedBytes += s.size
	}

	// Age pass.
	var survivors []sessionInfo
	for _, s := range sessions {
		if !s.active && s.newest.Before(cutoff) {
			remove(s)
			continue
		}
		survivors = append(survivors, s)
	}

	// Soft-cap pass: evict oldest non-active survivors until under cap.
	var total int64
	for _, s := range survivors {
		total += s.size
	}
	if total > opts.SoftCapBytes {
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].newest.Before(survivors[j].newest) })
		now := e.now()
		var kept []sessionInfo
		for i, s := range survivors {
			// Never evict the project's newest session, nor one with fresh writes:
			// "active" (live socket) misses degraded-mode and manual sessions, so
			// recency of the last write is the honest liveness signal.
			newestOfProject := i == len(survivors)-1
			if total > opts.SoftCapBytes && !s.active &&
				!newestOfProject && now.Sub(s.newest) > recentWriteGrace {
				remove(s)
				total -= s.size
				continue
			}
			kept = append(kept, s)
		}
		survivors = kept
	}

	for _, s := range survivors {
		rep.Kept = append(rep.Kept, s.id)
	}
	return rep, nil
}

func dirStats(dir string) (int64, time.Time) {
	var size int64
	var newest time.Time
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil // dir mtimes are noisy; the last write is a file mtime
		}
		size += info.Size()
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return size, newest
}
