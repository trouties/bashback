package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// decodeJSON fails the test if b is not a single JSON object with "v": 1.
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b)
	}
	if v, ok := m["v"].(float64); !ok || int(v) != outputVersion {
		t.Fatalf(`output missing "v": %d, got %v`, outputVersion, m["v"])
	}
	return m
}

func TestListJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "1")
	longCmd := "echo " + string(bytes.Repeat([]byte("x"), 200))
	f.capture(t, "tool_jsonlist", longCmd, func() { f.write(t, "b.txt", "2") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	entries, ok := m["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v", m["entries"])
	}
	e0 := entries[0].(map[string]any)
	if e0["key"] != "tool_jsonlist" {
		t.Fatalf("key = %v", e0["key"])
	}
	if e0["command"] != longCmd {
		t.Fatalf("command should be full (untruncated): %v", e0["command"])
	}
	if f := e0["files"].(float64); int(f) != 1 {
		t.Fatalf("files count = %v, want 1", e0["files"])
	}
}

func TestDiffJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "before\n")
	key := f.capture(t, "tool_jd", "x", func() { f.write(t, "f.txt", "after\n") })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{"--json", key}, &out, &errb); code != 0 {
		t.Fatalf("diff --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if m["key"] != key {
		t.Fatalf("key = %v", m["key"])
	}
	patch, _ := m["patch"].(string)
	if !bytes.Contains([]byte(patch), []byte("after")) {
		t.Fatalf("patch should contain the change: %q", patch)
	}
}

func TestRestoreJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	key := f.capture(t, "tool_jr", "x", func() { f.write(t, "f.txt", "changed") })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"--json", key}, &out, &errb); code != 0 {
		t.Fatalf("restore --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	for _, k := range []string{"key", "undo_key", "pre_restore_sha", "post_sha", "note", "files"} {
		if _, ok := m[k]; !ok {
			t.Errorf("restore --json missing %q", k)
		}
	}
	if m["key"] != key {
		t.Fatalf("key = %v, want %v", m["key"], key)
	}
	if m["undo_key"] == key || m["undo_key"] == "" {
		t.Fatalf("undo_key should be the new restore entry's key, got %v", m["undo_key"])
	}
}

func TestGCJSON(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := GC(f.layout, f.work, []string{"--json", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("gc --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if m["dry_run"] != true {
		t.Fatalf("dry_run = %v", m["dry_run"])
	}
}

func TestDoctorJSON(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, []string{"--json"}, &out, &errb)
	m := decodeJSON(t, out.Bytes())
	if _, ok := m["checks"].([]any); !ok {
		t.Fatalf("checks = %v", m["checks"])
	}
}

// --json must not change the error contract: errors stay text on stderr, non-zero code, stdout empty.
func TestErrorPathIgnoresJSON(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Restore(f.layout, f.work, []string{"--json", "nope"}, &out, &errb)
	if code == 0 {
		t.Fatal("unknown key should fail")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout should be empty on error, got %q", out.String())
	}
	if errb.Len() == 0 {
		t.Fatal("error should be on stderr")
	}
	var m map[string]any
	if json.Unmarshal(errb.Bytes(), &m) == nil {
		t.Fatalf("stderr should be plain text, not JSON: %q", errb.String())
	}
}

func TestRestoreDryRunCLI(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	key := f.capture(t, "tool_dry", "x", func() { f.write(t, "f.txt", "changed") })

	jbefore, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"--dry-run", key}, &out, &errb); code != 0 {
		t.Fatalf("dry-run exit %d: %s", code, errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("dry-run")) || !bytes.Contains(out.Bytes(), []byte("no changes made")) {
		t.Fatalf("dry-run output: %q", out.String())
	}
	// No file change, no new journal row.
	if b, _ := os.ReadFile(filepath.Join(f.work, "f.txt")); string(b) != "changed" {
		t.Fatalf("dry-run must not change the work-tree: %q", b)
	}
	jafter, _ := journal.ReadMerged(f.layout.JournalPath(f.work), journal.DefaultKeyer)
	if len(jafter) != len(jbefore) {
		t.Fatalf("dry-run must not journal: %d -> %d", len(jbefore), len(jafter))
	}
}

func TestRestoreDryRunJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	key := f.capture(t, "tool_dryj", "x", func() { f.write(t, "f.txt", "changed") })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"--dry-run", "--json", key}, &out, &errb); code != 0 {
		t.Fatalf("dry-run --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	plan, ok := m["plan"].(map[string]any)
	if !ok {
		t.Fatalf("plan = %v", m["plan"])
	}
	if _, ok := plan["checkout"]; !ok {
		t.Fatalf("plan missing checkout: %v", plan)
	}
	if m["mode"] != "three-class" {
		t.Fatalf("mode = %v", m["mode"])
	}
}
