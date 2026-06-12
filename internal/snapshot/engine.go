// Package snapshot is the use-case layer over gitx: fast-path probing, pre/post
// snapshotting, restore, and GC. Both the daemon and the degraded direct-write
// client drive it. It owns no concurrency control — callers serialize (daemon
// queue or lockfile).
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

const (
	// DefaultMaxFileBytes is the per-file snapshot ceiling: a single file over
	// this is excluded so build artifacts/model weights can't blow the 5s budget
	// or the disk cap.
	DefaultMaxFileBytes = int64(100) << 20
	// DefaultRetention and DefaultSoftCap drive GC.
	DefaultRetention    = 14 * 24 * time.Hour
	DefaultSoftCapBytes = int64(2) << 30
)

const (
	msgPre  = "bashback: pre"
	msgPost = "bashback: post"
)

// Engine performs snapshot operations against per-session shadow repos.
type Engine struct {
	Layout       paths.Layout
	Runner       gitx.Runner
	MaxFileBytes int64
	// MaxFileBytesFor, if set, resolves the per-project file ceiling (config
	// layer). nil keeps the built-in default / MaxFileBytes field (tests, bench).
	MaxFileBytesFor func(workdir string) int64
	// ProtectPathsFor, if set and returning a non-empty list, switches the repo
	// into sparse-protection mode: only files under the listed roots are
	// snapshotted; everything else is excluded and never dirties the fast-path
	// (config layer). nil = whole work-tree (the default).
	ProtectPathsFor func(workdir string) []string
	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// New builds an Engine with production defaults.
func New(layout paths.Layout, runner gitx.Runner) *Engine {
	return &Engine{
		Layout:       layout,
		Runner:       runner,
		MaxFileBytes: DefaultMaxFileBytes,
		Now:          time.Now,
	}
}

func (e *Engine) maxFileBytes(workdir string) int64 {
	if e.MaxFileBytesFor != nil {
		if b := e.MaxFileBytesFor(workdir); b > 0 {
			return b
		}
	}
	if e.MaxFileBytes > 0 {
		return e.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

// EnsureRepo creates (idempotently) the per-session shadow repo, its meta.json,
// and the forced `.git/` exclusion, returning a Repo ready to snapshot.
func (e *Engine) EnsureRepo(ctx context.Context, workdir, sessionID string) (*gitx.Repo, error) {
	if err := e.Layout.EnsureRepoDirs(workdir); err != nil {
		return nil, err
	}
	if _, err := e.Layout.ReadMeta(workdir); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		m := paths.Meta{SchemaVersion: paths.SchemaVersion, OriginalPath: workdir, CreatedAt: e.now().UTC().Format(time.RFC3339)}
		if err := e.Layout.WriteMeta(workdir, m); err != nil {
			return nil, err
		}
	}
	repo := gitx.NewRepo(e.Layout.SessionGitDir(workdir, sessionID), workdir, e.Runner)
	if !repo.Initialized() {
		if err := repo.Init(ctx); err != nil {
			return nil, err
		}
	}
	// The project's real .git must never be snapshotted.
	if err := repo.WriteExclude([]string{".git/"}); err != nil {
		return nil, err
	}
	// Sparse protection: a blanket `*` exclusion with per-root negations confines
	// the snapshot (and thus the fast-path status probe) to the listed paths.
	if e.ProtectPathsFor != nil {
		if pats := sparseExcludePatterns(e.ProtectPathsFor(workdir)); pats != nil {
			if err := repo.WriteExclude(pats); err != nil {
				return nil, err
			}
		}
	}
	return repo, nil
}

// sparseExcludePatterns builds the gitignore lines that confine a snapshot to the
// given roots: a blanket `*`, then a re-inclusion of each root's full parent
// chain (so git can descend) plus its contents. gitignore is last-match-wins, so
// the negations must follow `*`. Returns nil when no valid root remains. `.git`
// roots are dropped — the real repo is never protected.
func sparseExcludePatterns(dirs []string) []string {
	pats := []string{"*"}
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			pats = append(pats, p)
		}
	}
	for _, d := range dirs {
		d = strings.Trim(filepath.ToSlash(d), "/")
		if d == "" || d == ".git" || strings.HasPrefix(d, ".git/") {
			continue
		}
		prefix := ""
		for _, part := range strings.Split(d, "/") {
			if prefix == "" {
				prefix = part
			} else {
				prefix += "/" + part
			}
			add("!/" + prefix)
		}
		add("!/" + d + "/**")
	}
	if len(pats) == 1 {
		return nil
	}
	return pats
}

// RepoFor returns the shadow Repo for a (workdir, session) without creating it;
// callers that only read (diff) use this instead of EnsureRepo.
func (e *Engine) RepoFor(workdir, sessionID string) *gitx.Repo {
	return gitx.NewRepo(e.Layout.SessionGitDir(workdir, sessionID), workdir, e.Runner)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// PreResult carries what the caller needs to journal the pre side and feed Post.
type PreResult struct {
	PreSHA  string
	Skipped bool // fast-path hit: no new commit, HEAD reused
	Note    string
}

// PostResult carries the post sha and the final status.
type PostResult struct {
	PostSHA string
	Status  journal.Status
	Note    string
	// Files summarizes the pre..post changes for the journal; empty
	// on a fast-path skip (post == pre).
	Files        []journal.FileChange
	FilesOmitted int
}

// Pre takes the pre-command snapshot. On a clean work-tree with an existing
// baseline it reuses HEAD (fast-path) instead of committing; pre_sha is always
// populated so restore has a reference.
func (e *Engine) Pre(ctx context.Context, repo *gitx.Repo, forceInclude []string) (PreResult, error) {
	if repo.HasHead(ctx) {
		clean, err := repo.Clean(ctx)
		if err != nil {
			return PreResult{}, err
		}
		if clean {
			sha, err := repo.HeadSHA(ctx)
			if err != nil {
				return PreResult{}, err
			}
			return PreResult{PreSHA: sha, Skipped: true}, nil
		}
	}
	sha, committed, note, err := e.snap(ctx, repo, msgPre, forceInclude)
	if err != nil {
		return PreResult{}, err
	}
	return PreResult{PreSHA: sha, Skipped: !committed, Note: note}, nil
}

// Post takes the post-command snapshot and classifies the entry. skipped_no_change
// requires both the pre fast-path and an unchanged post; otherwise protected.
func (e *Engine) Post(ctx context.Context, repo *gitx.Repo, pre PreResult, forceInclude []string) (PostResult, error) {
	sha, committed, note, err := e.snap(ctx, repo, msgPost, forceInclude)
	if err != nil {
		return PostResult{}, err
	}
	status := journal.StatusProtected
	if pre.Skipped && !committed {
		status = journal.StatusSkippedNoChange
	}
	res := PostResult{PostSHA: sha, Status: status, Note: note}
	if sha != pre.PreSHA {
		diff, derr := repo.DiffNameStatus(ctx, pre.PreSHA, sha, nil)
		if derr != nil {
			return PostResult{}, derr
		}
		res.Files, res.FilesOmitted = summarizeFiles(diff)
	}
	return res, nil
}

// summarizeFiles converts a name-status diff into the capped journal file list,
// counting any overflow in the returned omitted count.
func summarizeFiles(diff []gitx.DiffEntry) ([]journal.FileChange, int) {
	if len(diff) == 0 {
		return nil, 0
	}
	omitted := 0
	if len(diff) > journal.FilesMax {
		omitted = len(diff) - journal.FilesMax
		diff = diff[:journal.FilesMax]
	}
	files := make([]journal.FileChange, len(diff))
	for i, d := range diff {
		files[i] = journal.FileChange{P: d.Path, S: d.Status}
	}
	return files, omitted
}

var (
	reOpenPerm = regexp.MustCompile(`open\("([^"]+)"\)`)
	reIndexErr = regexp.MustCompile(`unable to index file '([^']+)'`)
)

// snap performs one shadow snapshot: exclude oversized files, stage everything
// (retrying once around an unreadable file), force-include, then commit if there
// is anything new. Returns (sha, committedAnything, note).
// staleIndexLockAge is how old an index.lock must be before snap treats it as
// an orphan from a killed git and removes it. The caller holds the session's
// serialization (daemon queue / flock), so no live same-session git exists; a
// fresh lock is left alone as defense against out-of-band writers.
const staleIndexLockAge = 5 * time.Minute

func (e *Engine) recoverStaleIndexLock(repo *gitx.Repo, err error) bool {
	var ee *gitx.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "index.lock") {
		return false
	}
	lock := filepath.Join(repo.GitDir, "index.lock")
	fi, serr := os.Stat(lock)
	if serr != nil || time.Since(fi.ModTime()) < staleIndexLockAge {
		return false
	}
	return os.Remove(lock) == nil
}

func (e *Engine) snap(ctx context.Context, repo *gitx.Repo, msg string, forceInclude []string) (string, bool, string, error) {
	var notes []string

	// One full-tree status probe for the whole snapshot: its changed-path list
	// bounds the oversized scan. The post-add commit decision below is an
	// index-vs-HEAD probe, not a second status.
	changed, err := repo.ChangedPaths(ctx)
	if err != nil {
		return "", false, "", err
	}
	excluded, err := e.excludeOversized(ctx, repo, changed)
	if err != nil {
		return "", false, "", err
	}
	if len(excluded) > 0 {
		notes = append(notes, "excluded oversized: "+strings.Join(excluded, ", "))
	}

	if err := repo.AddAll(ctx); err != nil {
		if e.recoverStaleIndexLock(repo, err) {
			err = repo.AddAll(ctx)
		}
		if err != nil {
			// An unreadable file makes `add -A` fatal (exit 128). Exclude the
			// offending paths and retry once before giving up.
			bad := unreadablePaths(err)
			if len(bad) == 0 {
				return "", false, "", err
			}
			if werr := repo.WriteExclude(bad); werr != nil {
				return "", false, "", werr
			}
			notes = append(notes, "excluded unreadable: "+strings.Join(bad, ", "))
			if err := repo.AddAll(ctx); err != nil {
				return "", false, "", err
			}
		}
	}

	// Only force-include paths that currently exist; a deleted force-included
	// file is staged as a deletion by `add -A` above, while `add -f` on a
	// missing pathspec is fatal.
	if err := repo.AddForce(ctx, existingPaths(repo.WorkTree, forceInclude)); err != nil {
		return "", false, "", err
	}

	if gl, err := repo.GitlinkPaths(ctx); err == nil && len(gl) > 0 {
		notes = append(notes, "nested git (gitlink, content not captured): "+strings.Join(gl, ", "))
	}

	note := strings.Join(notes, "; ")

	baseline := !repo.HasHead(ctx)
	if baseline {
		sha, err := repo.CommitAllowEmpty(ctx, msg)
		return sha, true, note, err
	}
	// Index-vs-HEAD instead of a second full-tree status: after `add -A` /
	// `add -f`, "is there anything new to commit" is exactly the staged delta, and
	// this never rescans the work-tree. It also stays correct for
	// oversized exclusions and force-included ignored files, which a changed-paths
	// heuristic would miss.
	clean, err := repo.IndexClean(ctx)
	if err != nil {
		return "", false, note, err
	}
	if clean {
		sha, err := repo.HeadSHA(ctx)
		return sha, false, note, err
	}
	// Plumbing commit (write-tree/commit-tree) skips the porcelain commit's
	// internal full-tree status — the last status scan on the hot path.
	// Identical result: a child of HEAD holding the staged index.
	sha, err := repo.CommitTree(ctx, msg)
	return sha, true, note, err
}

// excludeOversized scans the given changed paths (the caller's single status
// probe), writing any file over the size ceiling to info/exclude so it stays out
// of the snapshot and the fast-path.
func (e *Engine) excludeOversized(_ context.Context, repo *gitx.Repo, changed []string) ([]string, error) {
	var over []string
	for _, rel := range changed {
		fi, err := os.Lstat(filepath.Join(repo.WorkTree, rel))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if fi.Size() > e.maxFileBytes(repo.WorkTree) {
			over = append(over, rel)
		}
	}
	if len(over) == 0 {
		return nil, nil
	}
	if err := repo.WriteExclude(over); err != nil {
		return nil, err
	}
	return over, nil
}

func existingPaths(workTree string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		if _, err := os.Lstat(filepath.Join(workTree, rel)); err == nil {
			out = append(out, rel)
		}
	}
	return out
}

func unreadablePaths(err error) []string {
	var ee *gitx.ExitError
	if !errors.As(err, &ee) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range append(reOpenPerm.FindAllStringSubmatch(ee.Stderr, -1), reIndexErr.FindAllStringSubmatch(ee.Stderr, -1)...) {
		if len(m) == 2 && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// ErrSnapshotReclaimed is returned by restore when the target snapshot's commits
// no longer exist (GC'd).
var ErrSnapshotReclaimed = errors.New("snapshot reclaimed by gc")

func reclaimedErr(sha string) error {
	return fmt.Errorf("%w: commit %s missing", ErrSnapshotReclaimed, sha)
}
