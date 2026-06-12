package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// A→B→C, then Rewind(A): the whole work-tree returns to A's pre instant —
// command-created files from A/B/C are gone and modified files revert.
func TestRewindWholeTreeToPre(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v0")

	entryA := h.capture(t, nil, func() {
		h.write(t, "fileA.txt", "A")
		h.write(t, "keep.txt", "v1")
	})
	h.capture(t, nil, func() {
		h.write(t, "fileB.txt", "B")
		h.write(t, "keep.txt", "v2")
	})
	h.capture(t, nil, func() {
		h.write(t, "fileC.txt", "C")
		h.removeFile(t, "fileA.txt")
	})

	rew, err := h.e.Rewind(ctx(), h.work, entryA, "span note")
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v0" {
		t.Fatalf("keep.txt = %q, want v0 (A's pre)", v)
	}
	for _, f := range []string{"fileA.txt", "fileB.txt", "fileC.txt"} {
		if _, ok := h.read(f); ok {
			t.Errorf("%s should be gone after rewind to A's pre", f)
		}
	}
	if rew.Status != journal.StatusRestored {
		t.Fatalf("rewind entry status = %s", rew.Status)
	}
	if rew.PreSHA == "" || rew.PostSHA == "" {
		t.Fatal("rewind entry needs pre/post for undoability")
	}
	if len(rew.Files) == 0 {
		t.Fatal("rewind entry should record the files it touched")
	}

	// The rewind is itself undoable: restoring it returns to the C state.
	if _, err := h.e.Restore(ctx(), h.work, rew, RestoreOpts{}); err != nil {
		t.Fatalf("undo rewind: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v2" {
		t.Fatalf("after undo of rewind keep.txt = %q, want v2", v)
	}
	if _, ok := h.read("fileC.txt"); !ok {
		t.Error("undo of rewind should restore fileC.txt")
	}
}

// Rewind works on a pre-only (interrupted) entry with no post snapshot.
func TestRewindPreOnly(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v0")
	entry := h.preCapture(t, func() {
		h.write(t, "keep.txt", "v1")
		h.write(t, "created.txt", "half")
	})
	if _, err := h.e.Rewind(ctx(), h.work, entry, ""); err != nil {
		t.Fatalf("rewind pre-only: %v", err)
	}
	if v, _ := h.read("keep.txt"); v != "v0" {
		t.Fatalf("keep.txt = %q, want v0", v)
	}
	if _, ok := h.read("created.txt"); ok {
		t.Error("created.txt should be removed")
	}
}

func TestRewindReclaimed(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "x")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "y") })
	entry.PreSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := h.e.Rewind(ctx(), h.work, entry, ""); err == nil || !isReclaimed(err) {
		t.Fatalf("want reclaimed, got %v", err)
	}
}

func (h *harness) removeFile(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(h.work, name)); err != nil {
		t.Fatal(err)
	}
}
