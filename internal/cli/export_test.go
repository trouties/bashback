package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// export streams a git-apply patch for an entry to stdout.
func TestExportStdout(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "before\n")
	key := f.capture(t, "tool_ex", "x", func() { f.write(t, "f.txt", "after\n") })

	var out, errb bytes.Buffer
	if code := Export(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("export exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "diff --git") || !strings.Contains(s, "+after") {
		t.Fatalf("export should emit a patch with the change: %q", s)
	}
}

// --out writes the patch to a file and stdout stays empty.
func TestExportOutFile(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "before\n")
	key := f.capture(t, "tool_exo", "x", func() { f.write(t, "f.txt", "after\n") })

	dst := filepath.Join(t.TempDir(), "patch.diff")
	var out, errb bytes.Buffer
	if code := Export(f.layout, f.work, []string{key, "--out", dst}, &out, &errb); code != 0 {
		t.Fatalf("export --out exit %d: %s", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("--out should keep stdout empty, got %q", out.String())
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(b), "diff --git") {
		t.Fatalf("out file should contain the patch: %q", b)
	}
}

// A pre-only (interrupted) entry has no post snapshot; export refuses and points
// at rewind.
func TestExportPreOnlyRefused(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1\n")
	key := f.preCapture(t, "tool_exp", "interrupted", func() { f.write(t, "f.txt", "half\n") })

	var out, errb bytes.Buffer
	if code := Export(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("export of a pre-only entry should refuse")
	}
	if !strings.Contains(errb.String(), "rewind") {
		t.Fatalf("pre-only export should guide to rewind, got %q", errb.String())
	}
}

// A reclaimed entry's snapshots are gone; export reports it with the standard
// reclaimed wording.
func TestExportReclaimed(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1\n")
	key := f.capture(t, "tool_exr", "x", func() { f.write(t, "f.txt", "v2\n") })
	if err := os.RemoveAll(f.layout.SessionGitDir(f.work, f.session)); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Export(f.layout, f.work, []string{key}, &out, &errb); code == 0 {
		t.Fatal("export of a reclaimed entry should fail")
	}
	if !strings.Contains(errb.String(), "reclaimed") {
		t.Fatalf("want reclaimed message, got %q", errb.String())
	}
}
