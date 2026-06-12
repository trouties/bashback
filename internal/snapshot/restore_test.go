package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// A restore against a gc'd session must report reclaimed without recreating the
// empty session repo (read/undo paths must never resurrect a shadow repo).
func TestRestoreOnReclaimedDoesNotResurrectRepo(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "v1")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "v2") })
	gitdir := h.e.Layout.SessionGitDir(h.work, h.session)
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); !errors.Is(err, ErrSnapshotReclaimed) {
		t.Fatalf("Restore err = %v, want ErrSnapshotReclaimed", err)
	}
	if _, err := os.Stat(gitdir); !os.IsNotExist(err) {
		t.Error("restore resurrected an empty session repo")
	}
}

// twentyLines builds a 20-line file with optional 1-based overrides, so two
// well-separated edits yield two distinct diff hunks.
func twentyLines(override map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		if v, ok := override[i]; ok {
			b.WriteString(v + "\n")
		} else {
			b.WriteString("line" + itoa(i) + "\n")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// firstHunkOnly returns the file header plus only the first hunk of a single-file
// patch (the snapshot package cannot import the cli hunk splitter).
func firstHunkOnly(patch string) string {
	var b strings.Builder
	hunks := 0
	for _, ln := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(ln, "@@ ") {
			hunks++
			if hunks == 2 {
				break
			}
		}
		b.WriteString(ln)
	}
	return b.String()
}

func (h *harness) patch(t *testing.T, e journal.Entry) string {
	t.Helper()
	r := h.e.RepoFor(h.work, e.SessionID)
	p, err := r.DiffPatch(ctx(), e.PreSHA, e.PostSHA, nil)
	if err != nil {
		t.Fatalf("DiffPatch: %v", err)
	}
	return string(p)
}

// A hunk subset reverts only the chosen region; the rest of the file keeps the
// command's result, and the new entry is marked partial.
func TestRestorePartialHunkSubset(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", twentyLines(nil))
	entry := h.capture(t, nil, func() {
		h.write(t, "f.txt", twentyLines(map[int]string{3: "FIRST", 17: "SECOND"}))
	})

	sel := PartialSelection{Patch: []byte(firstHunkOnly(h.patch(t, entry)))}
	restored, err := h.e.RestorePartial(ctx(), h.work, entry, sel, RestoreOpts{})
	if err != nil {
		t.Fatalf("RestorePartial: %v", err)
	}
	v, _ := h.read("f.txt")
	want := twentyLines(map[int]string{17: "SECOND"}) // first reverted, second kept
	if v != want {
		t.Fatalf("partial result =\n%q\nwant\n%q", v, want)
	}
	if !strings.Contains(restored.Note, "partial (interactive)") {
		t.Fatalf("note missing partial marker: %q", restored.Note)
	}
	if restored.Status != journal.StatusRestored {
		t.Fatalf("status = %s, want restored", restored.Status)
	}
}

// Checkout reverts a modified file from pre; Delete removes a command-created
// file and cleans its now-empty parent directory.
func TestRestorePartialCheckoutAndDelete(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	entry := h.capture(t, nil, func() {
		h.write(t, "keep.txt", "v2")
		h.write(t, "newdir/created.txt", "new")
	})

	sel := PartialSelection{Checkout: []string{"keep.txt"}, Delete: []string{"newdir/created.txt"}}
	if _, err := h.e.RestorePartial(ctx(), h.work, entry, sel, RestoreOpts{}); err != nil {
		t.Fatalf("RestorePartial: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v1" {
		t.Errorf("keep.txt = %q, want v1", v)
	}
	if _, ok := h.read("newdir/created.txt"); ok {
		t.Error("created file should be deleted")
	}
	if _, err := os.Stat(filepath.Join(h.work, "newdir")); !os.IsNotExist(err) {
		t.Errorf("empty parent dir should be removed, stat err=%v", err)
	}
}

// treeSnapshot maps every file under dir to its content for whole-tree equality.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A full selection (every text hunk via Patch, every command-created file via
// Delete) leaves the same work-tree as a whole Restore.
func TestRestorePartialFullSelectionEqualsRestore(t *testing.T) {
	setup := func(h *harness) journal.Entry {
		h.write(t, "mod.txt", "v1")
		h.write(t, "gone.txt", "doomed")
		return h.capture(t, nil, func() {
			h.write(t, "mod.txt", "v2")
			os.Remove(filepath.Join(h.work, "gone.txt"))
			h.write(t, "added.txt", "created")
		})
	}

	// Reference: a whole Restore.
	hr := newHarness(t)
	er := setup(hr)
	if _, err := hr.e.Restore(ctx(), hr.work, er, RestoreOpts{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Subject: a full-selection RestorePartial — the full patch (covers mod.txt and
	// the gone.txt re-creation) plus Delete of the command-created added.txt.
	hp := newHarness(t)
	ep := setup(hp)
	sel := PartialSelection{Patch: []byte(hp.patch(t, ep)), Delete: []string{"added.txt"}}
	if _, err := hp.e.RestorePartial(ctx(), hp.work, ep, sel, RestoreOpts{}); err != nil {
		t.Fatalf("RestorePartial full: %v", err)
	}

	got, want := treeSnapshot(t, hp.work), treeSnapshot(t, hr.work)
	if len(got) != len(want) {
		t.Fatalf("file sets differ: partial=%v restore=%v", keysOf(got), keysOf(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("file %q: partial=%q restore=%q", k, got[k], v)
		}
	}
}

func keysOf(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// A partial restore is itself undoable: restoring the new entry replays the
// command's result.
func TestRestorePartialUndoable(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", twentyLines(nil))
	entry := h.capture(t, nil, func() {
		h.write(t, "f.txt", twentyLines(map[int]string{3: "FIRST", 17: "SECOND"}))
	})
	post, _ := h.read("f.txt")

	sel := PartialSelection{Patch: []byte(firstHunkOnly(h.patch(t, entry)))}
	restored, err := h.e.RestorePartial(ctx(), h.work, entry, sel, RestoreOpts{})
	if err != nil {
		t.Fatalf("RestorePartial: %v", err)
	}
	if v, _ := h.read("f.txt"); v == post {
		t.Fatal("partial restore should have changed the work-tree")
	}
	if _, err := h.e.Restore(ctx(), h.work, restored, RestoreOpts{}); err != nil {
		t.Fatalf("undo partial: %v", err)
	}
	if v, _ := h.read("f.txt"); v != post {
		t.Fatalf("after undo = %q, want command result %q", v, post)
	}
}

// The overlapped gate fires before any work-tree change (no Force).
func TestRestorePartialGates(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", twentyLines(nil))
	entry := h.capture(t, nil, func() {
		h.write(t, "f.txt", twentyLines(map[int]string{3: "FIRST"}))
	})
	entry.Overlapped = true
	before, _ := h.read("f.txt")

	sel := PartialSelection{Patch: []byte(firstHunkOnly(h.patch(t, entry)))}
	if _, err := h.e.RestorePartial(ctx(), h.work, entry, sel, RestoreOpts{}); err != ErrOverlapped {
		t.Fatalf("want ErrOverlapped, got %v", err)
	}
	if after, _ := h.read("f.txt"); after != before {
		t.Fatal("gate must fire before any work-tree change")
	}
}

// Restore must handle non-ASCII paths end to end: revert a modified file and
// delete a command-created file whose names are CJK. Before quotepath=false the
// added-path diff returned a C-escaped name, so the created file silently
// survived the restore.
func TestRestoreHandlesNonASCIIPaths(t *testing.T) {
	h := newHarness(t)
	h.write(t, "café.yaml", "v1")
	entry := h.capture(t, nil, func() {
		h.write(t, "café.yaml", "v2")
		h.write(t, "résumé.txt", "created")
	})
	if _, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if v, _ := h.read("café.yaml"); v != "v1" {
		t.Errorf("café.yaml = %q, want v1 (restored)", v)
	}
	if _, ok := h.read("résumé.txt"); ok {
		t.Error("résumé.txt should be deleted (command-created file removed on restore)")
	}
}

// failPostRestoreCommit wraps a real runner and fails exactly the post-restore
// commit, so a test can drive the degraded-entry path without touching disk
// internals.
type failPostRestoreCommit struct{ inner gitx.Runner }

func (f failPostRestoreCommit) Run(ctx context.Context, args []string, opts gitx.RunOpts) (gitx.Result, error) {
	commit := false
	for _, a := range args {
		if a == "commit" || a == "commit-tree" {
			commit = true
		}
	}
	if commit {
		for _, a := range args {
			if strings.HasPrefix(a, "bashback: post-restore") {
				return gitx.Result{}, errors.New("simulated post-restore commit failure")
			}
		}
	}
	return f.inner.Run(ctx, args, opts)
}

func TestRestoreJournalsDegradedEntryWhenPostSnapFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".bashback")
	work := t.TempDir()
	e := New(paths.New(home), failPostRestoreCommit{inner: gitx.ExecRunner{}})
	h := &harness{e: e, work: work, session: "sess1"}
	h.write(t, "f.txt", "v1")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "v2") })

	restored, err := e.Restore(ctx(), work, entry, RestoreOpts{})
	if err == nil {
		t.Fatal("expected an error when the post-restore snapshot fails")
	}
	if restored.PreSHA == "" {
		t.Fatal("degraded entry must carry the recoverable pre-restore sha")
	}
	if restored.Status != journal.StatusRestored || !strings.Contains(restored.Note, "post-restore snapshot failed") {
		t.Fatalf("degraded entry = %+v, want restored status with a failure note", restored)
	}
}
