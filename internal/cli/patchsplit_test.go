package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thirtyLines builds a 30-line file overriding the given 1-based lines, so
// well-separated edits produce distinct diff hunks.
func thirtyLines(override map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		if v, ok := override[i]; ok {
			fmt.Fprintf(&b, "%s\n", v)
		} else {
			fmt.Fprintf(&b, "line%d\n", i)
		}
	}
	return b.String()
}

func patchFor(t *testing.T, f *fix, toolID string) string {
	t.Helper()
	e, ok, err := resolveEntry(f.layout, f.work, toolID)
	if err != nil || !ok {
		t.Fatalf("resolve %q: ok=%v err=%v", toolID, ok, err)
	}
	r := newEngine(f.layout).RepoFor(f.work, e.SessionID)
	patch, err := r.DiffPatch(ctx(), e.PreSHA, e.PostSHA, nil)
	if err != nil {
		t.Fatalf("DiffPatch: %v", err)
	}
	return string(patch)
}

// A two-file patch parses into two patchFiles: correct paths, one hunk each, preserved header.
func TestParsePatchMultiFile(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "a-old\n")
	f.write(t, "b.txt", "b-old\n")
	f.capture(t, "tool_m", "x", func() {
		f.write(t, "a.txt", "a-new\n")
		f.write(t, "b.txt", "b-new\n")
	})

	files := parsePatch(patchFor(t, f, "tool_m"))
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].Path != "a.txt" || files[1].Path != "b.txt" {
		t.Fatalf("paths = %q, %q", files[0].Path, files[1].Path)
	}
	for _, pf := range files {
		if len(pf.Hunks) != 1 {
			t.Fatalf("%s: want 1 hunk, got %d", pf.Path, len(pf.Hunks))
		}
		if !strings.HasPrefix(pf.Header, "diff --git ") {
			t.Fatalf("%s: header not preserved: %q", pf.Path, pf.Header)
		}
	}
}

// A binary file's segment is flagged Binary with no text hunks.
func TestParsePatchBinary(t *testing.T) {
	f := newFix(t)
	bin := filepath.Join(f.work, "b.bin")
	if err := os.WriteFile(bin, []byte{0, 1, 2, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	f.capture(t, "tool_b", "x", func() {
		if err := os.WriteFile(bin, []byte{255, 254, 0, 1, 2, 3}, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	e, _, _ := resolveEntry(f.layout, f.work, "tool_b")
	r := newEngine(f.layout).RepoFor(f.work, e.SessionID)
	patch, err := r.DiffPatchBinary(ctx(), e.PreSHA, e.PostSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := parsePatch(string(patch))
	var found bool
	for _, pf := range files {
		if pf.Path == "b.bin" {
			found = true
			if !pf.Binary {
				t.Fatalf("b.bin should be Binary")
			}
			if len(pf.Hunks) != 0 {
				t.Fatalf("binary file should have no hunks, got %d", len(pf.Hunks))
			}
		}
	}
	if !found {
		t.Fatalf("b.bin not parsed from:\n%s", patch)
	}
}

// Picking one of three hunks yields a patch with only that file's header and the
// chosen hunk, applying cleanly against the pre tree.
func TestAssembleSubset(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", thirtyLines(nil))
	f.capture(t, "tool_s", "x", func() {
		f.write(t, "f.txt", thirtyLines(map[int]string{3: "X3", 15: "X15", 27: "X27"}))
	})

	files := parsePatch(patchFor(t, f, "tool_s"))
	if len(files) != 1 || len(files[0].Hunks) != 3 {
		t.Fatalf("want 1 file / 3 hunks, got %d files / %d hunks", len(files), hunkCount(files))
	}

	subset := assemblePatch(files, func(fi, hi int) bool { return hi == 1 })
	if re := parsePatch(subset); len(re) != 1 || len(re[0].Hunks) != 1 {
		t.Fatalf("subset should hold exactly one file/one hunk, got %d/%d:\n%s", len(re), hunkCount(re), subset)
	}

	// Apply forward against the pre tree (stronger than `git apply --check`: it actually applies).
	e, _, _ := resolveEntry(f.layout, f.work, "tool_s")
	r := newEngine(f.layout).RepoFor(f.work, e.SessionID)
	if err := r.CheckoutPaths(ctx(), e.PreSHA, []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(ctx(), []byte(subset), false); err != nil {
		t.Fatalf("assembled subset did not apply: %v", err)
	}
}

func hunkCount(files []patchFile) int {
	n := 0
	for _, f := range files {
		n += len(f.Hunks)
	}
	return n
}

// Selecting nothing yields an empty patch.
func TestAssembleNothingEmpty(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", thirtyLines(nil))
	f.capture(t, "tool_n", "x", func() {
		f.write(t, "f.txt", thirtyLines(map[int]string{3: "X3", 15: "X15"}))
	})
	files := parsePatch(patchFor(t, f, "tool_n"))
	if got := assemblePatch(files, func(fi, hi int) bool { return false }); got != "" {
		t.Fatalf("empty selection should yield empty patch, got %q", got)
	}
}
