package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// identityArgs is injected before every git subcommand. Without a configured
// identity `commit` fails with "Author identity unknown"; gpgsign
// is forced off so a user's global signing config can't hang a hook on GPG.
// quotepath is forced off so non-ASCII paths come back as raw UTF-8 instead of
// C-escaped octal, which name-status parsing and restore pathspecs cannot use.
var identityArgs = []string{
	"-c", "user.name=bashback",
	"-c", "user.email=bashback@localhost",
	"-c", "commit.gpgsign=false",
	"-c", "core.quotepath=false",
}

// Repo is a shadow repository: an independent GIT_DIR whose work-tree is the
// project's cwd. It never writes a .git inside the work-tree.
type Repo struct {
	GitDir   string
	WorkTree string
	r        Runner
}

func NewRepo(gitDir, workTree string, r Runner) *Repo {
	return &Repo{GitDir: gitDir, WorkTree: workTree, r: r}
}

// baseArgs prepends the identity flags and the git-dir/work-tree separation so
// every operation targets the shadow repo regardless of process cwd.
func (rp *Repo) baseArgs(args ...string) []string {
	out := append([]string{}, identityArgs...)
	out = append(out, "--git-dir="+rp.GitDir, "--work-tree="+rp.WorkTree)
	return append(out, args...)
}

func (rp *Repo) run(ctx context.Context, opts RunOpts, args ...string) (Result, error) {
	return rp.r.Run(ctx, rp.baseArgs(args...), opts)
}

// Init creates the shadow GIT_DIR (idempotent) and enables the untracked cache
// for cheaper `status` on large work-trees.
func (rp *Repo) Init(ctx context.Context) error {
	if err := os.MkdirAll(rp.GitDir, 0o700); err != nil {
		return err
	}
	if _, err := rp.run(ctx, RunOpts{}, "init", "-q"); err != nil {
		return err
	}
	// Large-repo perf config, written only on fresh repos (callers gate on
	// Initialized so existing shadow repos are never silently migrated). All
	// best-effort: a failure only forgoes the optimization.
	for _, kv := range [][2]string{
		{"core.untrackedCache", "true"},
		{"index.version", "4"},
		{"feature.manyFiles", "true"},
	} {
		_, _ = rp.run(ctx, RunOpts{}, "config", kv[0], kv[1])
	}
	return nil
}

// Initialized reports whether the shadow GIT_DIR already holds a repo.
func (rp *Repo) Initialized() bool {
	_, err := os.Stat(filepath.Join(rp.GitDir, "HEAD"))
	return err == nil
}

// ConfigGet returns the value of a git config key in the shadow repo, or ""
// when unset or unreadable (doctor's perf-config probe; never an error path).
func (rp *Repo) ConfigGet(ctx context.Context, key string) string {
	res, err := rp.run(ctx, RunOpts{}, "config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

// Clean reports whether the work-tree matches the index/HEAD (empty porcelain
// output). This is the fast-path probe.
func (rp *Repo) Clean(ctx context.Context) (bool, error) {
	res, err := rp.run(ctx, RunOpts{}, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(res.Stdout))) == 0, nil
}

func (rp *Repo) AddAll(ctx context.Context) error {
	_, err := rp.run(ctx, RunOpts{}, "add", "-A")
	return err
}

// IndexClean reports whether the staged index matches HEAD (nothing new to
// commit). Uses `git diff --cached --quiet` so it never scans the work-tree:
// exit 0 = clean, exit 1 = staged differences, any other exit is a real fault.
func (rp *Repo) IndexClean(ctx context.Context) (bool, error) {
	_, err := rp.run(ctx, RunOpts{}, "diff", "--cached", "--quiet")
	if err == nil {
		return true, nil
	}
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 1 {
		return false, nil
	}
	return false, err
}

// AddForce stages paths even if ignored (force_include support).
func (rp *Repo) AddForce(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "-f", "--"}, paths...)
	_, err := rp.run(ctx, RunOpts{}, args...)
	return err
}

// HasHead reports whether the shadow repo has at least one commit.
func (rp *Repo) HasHead(ctx context.Context) bool {
	_, err := rp.run(ctx, RunOpts{}, "rev-parse", "--verify", "-q", "HEAD")
	return err == nil
}

// CommitAllowEmpty commits even with nothing staged, to establish a baseline
// commit on a fresh repo or empty work-tree.
func (rp *Repo) CommitAllowEmpty(ctx context.Context, message string) (string, error) {
	if _, err := rp.run(ctx, RunOpts{}, "commit", "-q", "--allow-empty", "-m", message); err != nil {
		return "", err
	}
	return rp.HeadSHA(ctx)
}

// CommitExists reports whether a commit-ish resolves in the shadow repo; used to
// detect snapshots reclaimed by GC before a restore.
func (rp *Repo) CommitExists(ctx context.Context, sha string) bool {
	_, err := rp.run(ctx, RunOpts{}, "rev-parse", "--verify", "-q", sha+"^{commit}")
	return err == nil
}

// ChangedPaths returns the paths reported by `status --porcelain` (untracked or
// modified), used to bound the oversized-file scan to what actually changed.
func (rp *Repo) ChangedPaths(ctx context.Context) ([]string, error) {
	// --no-renames keeps every record a single "XY <path>" segment: a rename would
	// otherwise emit a two-segment R record (old\x00new) and shift the path parse.
	res, err := rp.run(ctx, RunOpts{}, "status", "--porcelain", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rec := range strings.Split(string(res.Stdout), "\x00") {
		if len(rec) < 4 {
			continue
		}
		// Porcelain v1 -z: "XY <path>" with no quoting.
		out = append(out, rec[3:])
	}
	return out, nil
}

// GitlinkPaths lists embedded git repos recorded as gitlinks (mode 160000),
// whose content never enters the snapshot — a known restore blind spot.
func (rp *Repo) GitlinkPaths(ctx context.Context) ([]string, error) {
	res, err := rp.run(ctx, RunOpts{}, "ls-files", "-s")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if strings.HasPrefix(line, "160000 ") {
			// "160000 <sha> <stage>\t<path>"
			if i := strings.IndexByte(line, '\t'); i >= 0 {
				out = append(out, line[i+1:])
			}
		}
	}
	return out, nil
}

// Commit stages nothing on its own; call AddAll first. It returns the new HEAD
// sha. Committing with no staged changes is an error — callers gate on Clean.
func (rp *Repo) Commit(ctx context.Context, message string) (string, error) {
	if _, err := rp.run(ctx, RunOpts{}, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return rp.HeadSHA(ctx)
}

// CommitTree records the staged index as a child of HEAD via plumbing
// (write-tree/commit-tree/update-ref), bypassing the porcelain commit's status
// scan — the snapshot hot path on large work-trees. Requires a staged index
// (call AddAll first) and an existing HEAD parent; baseline commits use
// CommitAllowEmpty.
func (rp *Repo) CommitTree(ctx context.Context, message string) (string, error) {
	tree, err := rp.run(ctx, RunOpts{}, "write-tree")
	if err != nil {
		return "", err
	}
	treeSHA := strings.TrimSpace(string(tree.Stdout))
	parent, err := rp.HeadSHA(ctx)
	if err != nil {
		return "", err
	}
	res, err := rp.run(ctx, RunOpts{}, "commit-tree", treeSHA, "-p", parent, "-m", message)
	if err != nil {
		return "", err
	}
	commitSHA := strings.TrimSpace(string(res.Stdout))
	if _, err := rp.run(ctx, RunOpts{}, "update-ref", "HEAD", commitSHA); err != nil {
		return "", err
	}
	return commitSHA, nil
}

func (rp *Repo) HeadSHA(ctx context.Context) (string, error) {
	res, err := rp.run(ctx, RunOpts{}, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// CheckoutPaths restores the given paths from a snapshot into the work-tree. It
// does not remove files the snapshot lacks — that is the caller's job.
func (rp *Repo) CheckoutPaths(ctx context.Context, sha string, paths []string) error {
	args := append([]string{"checkout", sha, "--"}, paths...)
	_, err := rp.run(ctx, RunOpts{}, args...)
	return err
}

// DiffEntry is one line of `diff --name-status`.
type DiffEntry struct {
	Status string // A, M, D, R…
	Path   string
}

// DiffNameStatus lists path-level changes between two snapshots, optionally
// scoped to pathspecs. Rename detection is off so a rename reads as delete+add,
// which the three-class restore handles directly.
func (rp *Repo) DiffNameStatus(ctx context.Context, from, to string, paths []string) ([]DiffEntry, error) {
	args := []string{"diff", "--no-renames", "--name-status", from, to}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{}, args...)
	if err != nil {
		return nil, err
	}
	var out []DiffEntry
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		out = append(out, DiffEntry{Status: fields[0], Path: fields[1]})
	}
	return out, nil
}

// DiffNameStatusWorktree lists path-level changes between snapshot `sha` and the
// current work-tree, including untracked files, without touching the repo's real
// index. Used by restore --dry-run where there is no post commit.
//
// A plain `git diff <sha>` would miss untracked files, so we stage the work-tree
// into a scratch index (read-tree the snapshot, then `add -A`) and diff that
// against the snapshot, removing the scratch index after.
func (rp *Repo) DiffNameStatusWorktree(ctx context.Context, sha string, paths []string) ([]DiffEntry, error) {
	f, err := os.CreateTemp(rp.GitDir, "dryrun-index-*")
	if err != nil {
		return nil, err
	}
	idx := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(idx) }()

	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := rp.run(ctx, RunOpts{Env: env}, "read-tree", sha); err != nil {
		return nil, err
	}
	if _, err := rp.run(ctx, RunOpts{Env: env}, "add", "-A"); err != nil {
		return nil, err
	}
	args := []string{"diff", "--cached", "--no-renames", "--name-status", sha}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{Env: env}, args...)
	if err != nil {
		return nil, err
	}
	var out []DiffEntry
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		out = append(out, DiffEntry{Status: fields[0], Path: fields[1]})
	}
	return out, nil
}

// AddedPaths lists files that exist in `to` but not in `from` (files the command
// created). checkout won't delete these, so restore removes them explicitly.
func (rp *Repo) AddedPaths(ctx context.Context, from, to string, paths []string) ([]string, error) {
	args := []string{"diff", "--no-renames", "--diff-filter=A", "--name-only", from, to}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{}, args...)
	if err != nil {
		return nil, err
	}
	return splitLines(res.Stdout), nil
}

// NumStat is one line of `diff --numstat`. Added/Deleted are -1 for binary files
// (git prints "-" for those).
type NumStat struct {
	Added   int
	Deleted int
	Path    string
}

// DiffNumStat returns per-file added/deleted line counts between two snapshots
// (rename detection off, matching DiffNameStatus). Binary files report -1/-1.
func (rp *Repo) DiffNumStat(ctx context.Context, from, to string, paths []string) ([]NumStat, error) {
	args := []string{"diff", "--no-renames", "--numstat", from, to}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{}, args...)
	if err != nil {
		return nil, err
	}
	var out []NumStat
	for _, line := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		ns := NumStat{Path: fields[2], Added: -1, Deleted: -1}
		if fields[0] != "-" {
			_, _ = fmt.Sscanf(fields[0], "%d", &ns.Added)
		}
		if fields[1] != "-" {
			_, _ = fmt.Sscanf(fields[1], "%d", &ns.Deleted)
		}
		out = append(out, ns)
	}
	return out, nil
}

// WorktreeChangedSince reports whether the work-tree differs from snapshot `sha`
// over the given paths — i.e. the user touched them after the post snapshot.
// Implemented via `diff --quiet`, whose exit code 1 means "differences".
func (rp *Repo) WorktreeChangedSince(ctx context.Context, sha string, paths []string) (bool, error) {
	args := []string{"diff", "--quiet", sha}
	args = appendPathspec(args, paths)
	_, err := rp.run(ctx, RunOpts{}, args...)
	if err == nil {
		return false, nil
	}
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 1 {
		return true, nil
	}
	return false, err
}

// DiffPatch produces the textual patch from `from` to `to` for the given paths,
// suitable for `apply --3way [-R]`.
func (rp *Repo) DiffPatch(ctx context.Context, from, to string, paths []string) ([]byte, error) {
	args := []string{"diff", from, to}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{}, args...)
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// DiffPatchBinary is DiffPatch with --binary: the patch carries binary file
// contents so `git apply` round-trips them faithfully (the plain text patch drops
// binary hunks and cannot be replayed). Used by `export`.
func (rp *Repo) DiffPatchBinary(ctx context.Context, from, to string, paths []string) ([]byte, error) {
	args := []string{"diff", "--binary", from, to}
	args = appendPathspec(args, paths)
	res, err := rp.run(ctx, RunOpts{}, args...)
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// Apply applies a plain patch to the work-tree (no three-way fallback); reverse
// undoes it. Unlike Apply3Way it fails outright rather than inserting conflict
// markers, which is exactly what hunk-level restore needs: a hand-assembled
// subset patch must apply cleanly or be rejected, never half-merged.
func (rp *Repo) Apply(ctx context.Context, patch []byte, reverse bool) error {
	args := []string{"apply"}
	if reverse {
		args = append(args, "-R")
	}
	// `git apply` resolves patch paths relative to the process cwd, not the
	// --work-tree flag, so run it from inside the work-tree.
	_, err := rp.run(ctx, RunOpts{Stdin: patch, Dir: rp.WorkTree}, args...)
	return err
}

// Apply3Way applies a patch with three-way merge fallback; reverse undoes it.
// Conflicts surface as standard conflict markers rather than silent overwrites.
func (rp *Repo) Apply3Way(ctx context.Context, patch []byte, reverse bool) error {
	args := []string{"apply", "--3way"}
	if reverse {
		args = append(args, "-R")
	}
	// `git apply` resolves patch paths relative to the process cwd, not the
	// --work-tree flag, so run it from inside the work-tree.
	_, err := rp.run(ctx, RunOpts{Stdin: patch, Dir: rp.WorkTree}, args...)
	return err
}

// WriteExclude appends patterns to $GIT_DIR/info/exclude. Exclusion must live
// here (not in `add` pathspecs) or the file stays forever untracked in `status`
// and the fast-path breaks permanently.
func (rp *Repo) WriteExclude(patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}
	dir := filepath.Join(rp.GitDir, "info")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "exclude")
	existing, _ := os.ReadFile(path)
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for _, p := range patterns {
		if p == "" || have[p] {
			continue
		}
		if _, err := fmt.Fprintln(f, p); err != nil {
			return err
		}
	}
	return nil
}

func appendPathspec(args, paths []string) []string {
	if len(paths) == 0 {
		return args
	}
	args = append(args, "--")
	return append(args, paths...)
}

func splitLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
