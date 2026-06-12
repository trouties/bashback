package gitx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DiffPatchBinary emits a --binary patch that round-trips a binary file change
// through `git apply` (the plain text patch drops binary hunks) — the basis of
// `export`.
func TestDiffPatchBinaryRoundTrip(t *testing.T) {
	rp, work := newTestRepo(t)
	v0 := []byte{0, 1, 2, 3, 0, 255, 7}
	v1 := []byte{255, 0, 9, 8, 7, 0, 1, 2}
	if err := os.WriteFile(filepath.Join(work, "bin"), v0, 0o644); err != nil {
		t.Fatal(err)
	}
	pre := snapshot(t, rp, "pre")
	if err := os.WriteFile(filepath.Join(work, "bin"), v1, 0o644); err != nil {
		t.Fatal(err)
	}
	post := snapshot(t, rp, "post")

	patch, err := rp.DiffPatchBinary(ctx(), pre, post, nil)
	if err != nil {
		t.Fatalf("DiffPatchBinary: %v", err)
	}
	if !strings.Contains(string(patch), "GIT binary patch") {
		t.Fatalf("expected a git binary patch, got:\n%s", patch)
	}

	// Round-trip: reset the work-tree to pre, apply forward, expect post bytes.
	if err := rp.CheckoutPaths(ctx(), pre, []string{"."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "bin")); !bytes.Equal(got, v0) {
		t.Fatalf("checkout pre did not restore v0, got %v", got)
	}
	if err := rp.Apply3Way(ctx(), patch, false); err != nil {
		t.Fatalf("apply binary patch: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(work, "bin"))
	if !bytes.Equal(got, v1) {
		t.Fatalf("after applying binary patch, bin = %v, want %v", got, v1)
	}
}

// A freshly-initialized shadow repo carries the large-repo git performance
// config: untracked cache, index v4, and manyFiles.
func TestInitWritesPerfConfig(t *testing.T) {
	rp, _ := newTestRepo(t) // newTestRepo calls Init
	want := map[string]string{
		"core.untrackedCache": "true",
		"index.version":       "4",
		"feature.manyFiles":   "true",
	}
	for k, v := range want {
		res, err := rp.run(ctx(), RunOpts{}, "config", "--get", k)
		if err != nil {
			t.Fatalf("config --get %s: %v", k, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != v {
			t.Errorf("config %s = %q, want %q", k, got, v)
		}
	}
}

func ctx() context.Context { return context.Background() }

// ConfigGet reads back what Init wrote, and returns "" once unset — doctor's
// perf-config probe depends on both.
func TestConfigGet(t *testing.T) {
	rp, _ := newTestRepo(t) // Init already ran (idempotent)
	if got := rp.ConfigGet(ctx(), "index.version"); got != "4" {
		t.Fatalf("fresh repo index.version = %q, want 4", got)
	}
	if _, err := rp.run(ctx(), RunOpts{}, "config", "--unset", "index.version"); err != nil {
		t.Fatal(err)
	}
	if got := rp.ConfigGet(ctx(), "index.version"); got != "" {
		t.Fatalf("unset key = %q, want empty", got)
	}
}

func newTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	work := t.TempDir()
	gitDir := filepath.Join(t.TempDir(), "shadow.git")
	rp := NewRepo(gitDir, work, ExecRunner{})
	if err := rp.Init(ctx()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return rp, work
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, rp *Repo, msg string) string {
	t.Helper()
	if err := rp.AddAll(ctx()); err != nil {
		t.Fatalf("add: %v", err)
	}
	sha, err := rp.Commit(ctx(), msg)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return sha
}

// E1: commit must succeed with no ambient git identity (ExecRunner pins config
// to /dev/null; the -c identity injection carries it).
func TestCommitWithoutAmbientIdentity(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "a.txt", "hello")
	sha := snapshot(t, rp, "snap")
	if len(sha) < 7 {
		t.Fatalf("bad sha %q", sha)
	}
	// The shadow GIT_DIR must hold the repo; the work-tree must stay clean of .git.
	if _, err := os.Stat(filepath.Join(work, ".git")); !os.IsNotExist(err) {
		t.Fatalf("work-tree should have no .git, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rp.GitDir, "HEAD")); err != nil {
		t.Fatalf("shadow GIT_DIR missing HEAD: %v", err)
	}
}

// CommitTree records the staged index as a child of HEAD via plumbing, moving
// HEAD, without the porcelain commit's internal status scan.
func TestCommitTree(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "a.txt", "v0")
	base := snapshot(t, rp, "base")

	write(t, work, "a.txt", "v1")
	if err := rp.AddAll(ctx()); err != nil {
		t.Fatal(err)
	}
	sha, err := rp.CommitTree(ctx(), "snap")
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	if sha == base || len(sha) < 7 {
		t.Fatalf("commit-tree should create a new commit, got %q (base %q)", sha, base)
	}
	if head, _ := rp.HeadSHA(ctx()); head != sha {
		t.Fatalf("HEAD = %q, want new commit %q", head, sha)
	}
	parent, _ := rp.run(ctx(), RunOpts{}, "rev-parse", sha+"^")
	if got := strings.TrimSpace(string(parent.Stdout)); got != base {
		t.Fatalf("parent = %q, want base %q", got, base)
	}
	content, _ := rp.run(ctx(), RunOpts{}, "show", sha+":a.txt")
	if got := strings.TrimSpace(string(content.Stdout)); got != "v1" {
		t.Fatalf("committed content = %q, want v1", got)
	}
}

func TestCleanFastPath(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "a.txt", "x")
	if clean, _ := rp.Clean(ctx()); clean {
		t.Fatal("untracked file should make repo dirty")
	}
	snapshot(t, rp, "snap")
	if clean, err := rp.Clean(ctx()); err != nil || !clean {
		t.Fatalf("after commit want clean, got clean=%v err=%v", clean, err)
	}
}

// IndexClean reports whether the staged index matches HEAD, independent of the
// untracked work-tree — the cheap post-add commit-decision probe.
func TestIndexCleanVsHead(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "a.txt", "v0")
	snapshot(t, rp, "base")

	// Right after a commit nothing is staged → index matches HEAD.
	if clean, err := rp.IndexClean(ctx()); err != nil || !clean {
		t.Fatalf("post-commit index should be clean, got clean=%v err=%v", clean, err)
	}

	// An unstaged work-tree edit does not dirty the index.
	write(t, work, "a.txt", "v1")
	if clean, err := rp.IndexClean(ctx()); err != nil || !clean {
		t.Fatalf("unstaged edit must not dirty the index, got clean=%v err=%v", clean, err)
	}

	// Staging it does.
	if err := rp.AddAll(ctx()); err != nil {
		t.Fatal(err)
	}
	if clean, err := rp.IndexClean(ctx()); err != nil || clean {
		t.Fatalf("staged change should make index dirty, got clean=%v err=%v", clean, err)
	}
}

func TestCheckoutRestoresModifiedAndDeleted(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "keep.txt", "v1")
	write(t, work, "sub/del.txt", "orig")
	pre := snapshot(t, rp, "pre")

	write(t, work, "keep.txt", "v2-modified")
	if err := os.Remove(filepath.Join(work, "sub/del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := rp.CheckoutPaths(ctx(), pre, []string{"."}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "keep.txt")); string(b) != "v1" {
		t.Errorf("keep.txt = %q, want v1", b)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "sub/del.txt")); string(b) != "orig" {
		t.Errorf("deleted file not restored: %q", b)
	}
}

func TestAddedPathsAndDiffNameStatus(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "f1.txt", "a")
	pre := snapshot(t, rp, "pre")
	write(t, work, "f2.txt", "b")
	write(t, work, "f1.txt", "a-changed")
	post := snapshot(t, rp, "post")

	added, err := rp.AddedPaths(ctx(), pre, post, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "f2.txt" {
		t.Fatalf("AddedPaths = %v, want [f2.txt]", added)
	}
	entries, err := rp.DiffNameStatus(ctx(), pre, post, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Path] = e.Status
	}
	if got["f1.txt"] != "M" || got["f2.txt"] != "A" {
		t.Fatalf("name-status = %v, want f1=M f2=A", got)
	}
}

func TestWorktreeChangedSince(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "f.txt", "a")
	sha := snapshot(t, rp, "snap")
	if changed, err := rp.WorktreeChangedSince(ctx(), sha, nil); err != nil || changed {
		t.Fatalf("clean tree: changed=%v err=%v", changed, err)
	}
	write(t, work, "f.txt", "a-edited")
	if changed, err := rp.WorktreeChangedSince(ctx(), sha, nil); err != nil || !changed {
		t.Fatalf("edited tree: changed=%v err=%v", changed, err)
	}
}

// E3: reverse-applying the pre..post patch undoes the change when the work-tree
// matches the index (the pre-restore snapshot guarantees that alignment).
func TestApply3WayReverse(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "f.txt", "line1\n")
	pre := snapshot(t, rp, "pre")
	write(t, work, "f.txt", "line1\nline2\n")
	post := snapshot(t, rp, "post")

	patch, err := rp.DiffPatch(ctx(), pre, post, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.Apply3Way(ctx(), patch, true); err != nil {
		t.Fatalf("reverse apply: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "f.txt")); string(b) != "line1\n" {
		t.Fatalf("after reverse apply = %q, want line1", b)
	}
}

// Apply round-trips a plain patch: reverse takes the work-tree from B back to A,
// forward replays A to B (the basis of hunk-level restore).
func TestApplyReverse(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "f.txt", "A\n")
	a := snapshot(t, rp, "A")
	write(t, work, "f.txt", "B\n")
	b := snapshot(t, rp, "B")

	patch, err := rp.DiffPatch(ctx(), a, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.Apply(ctx(), patch, true); err != nil {
		t.Fatalf("reverse apply: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "f.txt")); string(got) != "A\n" {
		t.Fatalf("after reverse = %q, want A", got)
	}
	if err := rp.Apply(ctx(), patch, false); err != nil {
		t.Fatalf("forward apply: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "f.txt")); string(got) != "B\n" {
		t.Fatalf("after forward = %q, want B", got)
	}
}

// E7: exclusion via info/exclude keeps the file out of `status`, so the
// fast-path keeps working (an `add` pathspec would not).
func TestWriteExcludeKeepsStatusClean(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "base.txt", "x")
	snapshot(t, rp, "base")
	write(t, work, "model.bin", "huge")
	if clean, _ := rp.Clean(ctx()); clean {
		t.Fatal("untracked model.bin should be dirty before exclude")
	}
	if err := rp.WriteExclude([]string{"model.bin"}); err != nil {
		t.Fatal(err)
	}
	if clean, err := rp.Clean(ctx()); err != nil || !clean {
		t.Fatalf("after exclude want clean, got clean=%v err=%v", clean, err)
	}
}

// E1: .gitignore is honored — an ignored file never dirties the shadow status.
func TestGitignoreHonored(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, ".gitignore", ".env\n")
	write(t, work, ".env", "SECRET=1")
	snapshot(t, rp, "base")
	write(t, work, ".env", "SECRET=changed")
	if clean, err := rp.Clean(ctx()); err != nil || !clean {
		t.Fatalf("ignored .env must not dirty status: clean=%v err=%v", clean, err)
	}
}

func TestDetectVersionMeetsMinimum(t *testing.T) {
	v, err := DetectVersion(ctx(), ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.MeetsMinimum() {
		t.Fatalf("git %s below required %d.%d", v.Raw, MinMajor, MinMinor)
	}
}

// Non-ASCII paths must survive name-status parsing: core.quotepath=false keeps
// git from C-escaping them into a quoted, unusable form.
func TestDiffNameStatusNonASCIIPath(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "café-tëst.txt", "v1")
	pre := snapshot(t, rp, "pre")
	write(t, work, "café-tëst.txt", "v2")
	post := snapshot(t, rp, "post")

	diff, err := rp.DiffNameStatus(ctx(), pre, post, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 1 || diff[0].Path != "café-tëst.txt" {
		t.Fatalf("diff = %+v, want unescaped path café-tëst.txt", diff)
	}
}

// ChangedPaths must never emit a garbage path: with --no-renames a staged rename
// stays two single-segment records, so the path parse can't slice into the wrong
// field (an R record's trailing old-name segment).
func TestChangedPathsHandlesRenameRecords(t *testing.T) {
	rp, work := newTestRepo(t)
	write(t, work, "original.txt", "stable content for rename detection\n")
	snapshot(t, rp, "base")

	if err := os.Remove(filepath.Join(work, "original.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, work, "renamed.txt", "stable content for rename detection\n")
	if err := rp.AddAll(ctx()); err != nil {
		t.Fatal(err)
	}

	paths, err := rp.ChangedPaths(ctx())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if p != "original.txt" && p != "renamed.txt" {
			t.Fatalf("ChangedPaths returned a garbage path %q (want only original.txt/renamed.txt): %v", p, paths)
		}
	}
}
