package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/snapshot"
)

func TestSnapCreatesManualEntry(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")

	var out, errb bytes.Buffer
	if code := Snap(f.layout, f.work, []string{"-m", "before risky op"}, &out, &errb); code != 0 {
		t.Fatalf("snap exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "snap_") || !strings.Contains(out.String(), "rewind") {
		t.Fatalf("snap output: %q", out.String())
	}
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Status != journal.StatusManual || e.SessionID != snapshot.ManualSessionID {
		t.Fatalf("snap entry = %+v", e)
	}
	if !strings.HasPrefix(e.ToolUseID, "snap_") || e.PostSHA != "" {
		t.Fatalf("snap should be a pre-only snap_* entry: %+v", e)
	}
	if !strings.Contains(e.Command, "risky") {
		t.Fatalf("message not stored: %q", e.Command)
	}
}

// A manual snap is pre-only by design: `list` shows a clean `manual` (not the
// interrupted annotation) and `show` steers to rewind, not the restore --force
// path that manual checkpoints refuse.
func TestSnapNotFlaggedInterrupted(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")

	var out, errb bytes.Buffer
	if code := Snap(f.layout, f.work, []string{"-m", "checkpoint"}, &out, &errb); code != 0 {
		t.Fatalf("snap exit %d: %s", code, errb.String())
	}
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	key := entries[0].ToolUseID

	out.Reset()
	errb.Reset()
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if s := out.String(); !strings.Contains(s, "manual") || strings.Contains(s, "interrupted") {
		t.Fatalf("manual snap must show clean `manual`, no interrupted flag: %q", s)
	}

	out.Reset()
	errb.Reset()
	if code := Show(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	s := out.String()
	if strings.Contains(s, "interrupted") || strings.Contains(s, "restore "+key+" --force") {
		t.Fatalf("manual snap show must not offer the interrupted restore --force path: %q", s)
	}
	if !strings.Contains(s, "rewind "+key) {
		t.Fatalf("manual snap show must point at rewind: %q", s)
	}
}

// Fast-path: a second snap with no work-tree change reuses the SHA and notes it.
func TestSnapFastPathNoChange(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	var out, errb bytes.Buffer
	Snap(f.layout, f.work, nil, &out, &errb)
	out.Reset()
	errb.Reset()
	if code := Snap(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "no changes since last snapshot") {
		t.Fatalf("second snap should note no changes: %q", out.String())
	}
}

// restore of a snap key is refused and points at rewind.
func TestRestoreSnapRefused(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	var out, errb bytes.Buffer
	Snap(f.layout, f.work, nil, &out, &errb)
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	snapKey := entries[0].ToolUseID

	out.Reset()
	errb.Reset()
	if code := Restore(f.layout, f.work, []string{snapKey}, &out, &errb); code == 0 {
		t.Fatal("restore of a snap should refuse")
	}
	if !strings.Contains(errb.String(), "rewind") {
		t.Fatalf("restore should point at rewind: %q", errb.String())
	}
}

// rewind of a snap key round-trips: snap, change, rewind restores the snap state.
func TestRewindSnapRoundTrip(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "checkpoint")

	var out, errb bytes.Buffer
	Snap(f.layout, f.work, []string{"-m", "cp"}, &out, &errb)
	entries, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	snapKey := entries[0].ToolUseID

	// A change after the snap (must be captured so it isn't an "uncommitted" gate).
	f.capture(t, "tool_after", "danger", func() { f.write(t, "f.txt", "DESTROYED") })

	out.Reset()
	errb.Reset()
	if code := Rewind(f.layout, f.work, []string{snapKey}, &out, &errb); code != 0 {
		t.Fatalf("rewind snap exit %d: %s", code, errb.String())
	}
	if v := readFile(t, f, "f.txt"); v != "checkpoint" {
		t.Fatalf("rewind to snap should restore checkpoint, got %q", v)
	}
}

// Concurrent snaps serialize on the flock and produce a valid journal (no loss / torn writes).
func TestSnapConcurrent(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, errb bytes.Buffer
			if code := Snap(f.layout, f.work, nil, &out, &errb); code != 0 {
				t.Errorf("concurrent snap failed: %s", errb.String())
			}
		}()
	}
	wg.Wait()
	// Journal must be readable (no torn lines).
	entries, err := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if err != nil {
		t.Fatalf("journal corrupted by concurrent snaps: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no snap recorded")
	}
	for _, e := range entries {
		if e.Status != journal.StatusManual {
			t.Fatalf("unexpected entry: %+v", e)
		}
	}
}
