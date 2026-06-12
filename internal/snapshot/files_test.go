package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// fileSet collapses a Files slice to a path->status map for order-insensitive
// assertions.
func fileSet(files []journal.FileChange) map[string]string {
	m := map[string]string{}
	for _, f := range files {
		m[f.P] = f.S
	}
	return m
}

func TestPostFilesSummary(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	h.write(t, "gone.txt", "bye")
	entry := h.capture(t, nil, func() {
		h.write(t, "keep.txt", "v2")                 // M
		os.Remove(filepath.Join(h.work, "gone.txt")) // D
		h.write(t, "new.txt", "hi")                  // A
	})
	got := fileSet(entry.Files)
	want := map[string]string{"keep.txt": "M", "gone.txt": "D", "new.txt": "A"}
	if len(got) != len(want) {
		t.Fatalf("files = %+v, want %+v", got, want)
	}
	for p, s := range want {
		if got[p] != s {
			t.Errorf("%s status = %q, want %q", p, got[p], s)
		}
	}
	if entry.FilesOmitted != 0 {
		t.Errorf("files_omitted = %d, want 0", entry.FilesOmitted)
	}
}

func TestPostFilesEmptyOnSkip(t *testing.T) {
	h := newHarness(t)
	h.write(t, "a.txt", "v1")
	h.capture(t, nil, nil) // baseline
	noop := h.capture(t, nil, nil)
	if noop.Status != journal.StatusSkippedNoChange {
		t.Fatalf("status = %s", noop.Status)
	}
	if len(noop.Files) != 0 {
		t.Fatalf("skipped entry must not record files: %+v", noop.Files)
	}
}

func TestPostFilesCapAndOmitted(t *testing.T) {
	h := newHarness(t)
	h.write(t, "base.txt", "x")
	h.capture(t, nil, nil) // baseline
	const total = journal.FilesMax + 5
	entry := h.capture(t, nil, func() {
		for i := 0; i < total; i++ {
			h.write(t, fmt.Sprintf("f%03d.txt", i), "data")
		}
	})
	if len(entry.Files) != journal.FilesMax {
		t.Fatalf("files len = %d, want %d", len(entry.Files), journal.FilesMax)
	}
	if entry.FilesOmitted != total-journal.FilesMax {
		t.Fatalf("files_omitted = %d, want %d", entry.FilesOmitted, total-journal.FilesMax)
	}
}

// Rename must decompose to A+D (not R<score>) thanks to --no-renames, so the
// three-class restore handles it directly.
func TestPostFilesRenameDecomposed(t *testing.T) {
	h := newHarness(t)
	h.write(t, "old.txt", "stable content here")
	h.capture(t, nil, nil) // baseline
	entry := h.capture(t, nil, func() {
		os.Rename(filepath.Join(h.work, "old.txt"), filepath.Join(h.work, "new.txt"))
	})
	got := fileSet(entry.Files)
	if got["old.txt"] != "D" || got["new.txt"] != "A" {
		t.Fatalf("rename not decomposed to A+D: %+v", got)
	}
	for _, f := range entry.Files {
		if f.S == "R" || len(f.S) > 1 {
			t.Fatalf("unexpected rename status %q", f.S)
		}
	}
}

func TestRestoreEntryRecordsFiles(t *testing.T) {
	h := newHarness(t)
	h.write(t, "a.txt", "a1")
	h.write(t, "b.txt", "b1")
	entry := h.capture(t, nil, func() {
		h.write(t, "a.txt", "a2")
		h.write(t, "b.txt", "b2")
	})
	restored, err := h.e.Restore(ctx(), h.work, entry, RestoreOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got := fileSet(restored.Files)
	if got["a.txt"] == "" || got["b.txt"] == "" {
		t.Fatalf("restore entry should record the reverted files: %+v", restored.Files)
	}
}
