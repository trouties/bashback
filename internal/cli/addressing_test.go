package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// Same-second entries order newest-first by journal row order (later-written row =
// later event), so @N addressing is deterministic even within one second.
func TestTsDescOrderSameSecondNewestRowFirst(t *testing.T) {
	const ts = "2026-06-10T00:00:05Z"
	entries := []journal.Entry{
		{ToolUseID: "e1", TS: ts},
		{ToolUseID: "e2", TS: ts},
		{ToolUseID: "e3", TS: ts},
	}
	ordered := tsDescOrder(entries)
	got := []string{ordered[0].ToolUseID, ordered[1].ToolUseID, ordered[2].ToolUseID}
	want := []string{"e3", "e2", "e1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("same-second tsDescOrder = %v, want %v", got, want)
		}
	}

	// Different seconds: strict ts-descending, unaffected by row order.
	mixed := []journal.Entry{
		{ToolUseID: "old", TS: "2026-06-10T00:00:01Z"},
		{ToolUseID: "new", TS: "2026-06-10T00:00:09Z"},
		{ToolUseID: "mid", TS: "2026-06-10T00:00:05Z"},
	}
	om := tsDescOrder(mixed)
	if om[0].ToolUseID != "new" || om[1].ToolUseID != "mid" || om[2].ToolUseID != "old" {
		t.Fatalf("different-second order broken: %v", []string{om[0].ToolUseID, om[1].ToolUseID, om[2].ToolUseID})
	}
}

// @1 resolves to the last-written row among same-second entries.
func TestAtNSameSecondResolvesNewestRow(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	jp := f.layout.JournalPath(f.work)
	for _, id := range []string{"first", "second", "third"} {
		if err := journal.Append(jp, journal.Entry{
			ToolUseID: id, SessionID: f.session,
			TS:     "2026-06-10T00:00:05Z",
			PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
		}); err != nil {
			t.Fatal(err)
		}
	}
	e, ok, err := resolveEntry(f.layout, f.work, "@1")
	if err != nil || !ok {
		t.Fatalf("resolve @1: ok=%v err=%v", ok, err)
	}
	if e.ToolUseID != "third" {
		t.Fatalf("@1 = %q, want last-written row %q", e.ToolUseID, "third")
	}
	e3, ok, err := resolveEntry(f.layout, f.work, "@3")
	if err != nil || !ok || e3.ToolUseID != "first" {
		t.Fatalf("@3 = %q ok=%v err=%v, want first-written row", e3.ToolUseID, ok, err)
	}
}

// @1 resolves to the newest entry and is equivalent to passing its full key.
func TestAtNResolvesNewest(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_old", "first", func() { f.write(t, "f.txt", "v1") })
	newest := f.capture(t, "tool_new", "second", func() { f.write(t, "f.txt", "v2") })

	e, ok, err := resolveEntry(f.layout, f.work, "@1")
	if err != nil || !ok {
		t.Fatalf("resolve @1: ok=%v err=%v", ok, err)
	}
	if e.ToolUseID != newest {
		t.Fatalf("@1 = %q, want newest %q", e.ToolUseID, newest)
	}

	e2, ok, err := resolveEntry(f.layout, f.work, "@2")
	if err != nil || !ok || e2.ToolUseID != "tool_old" {
		t.Fatalf("@2 = %q ok=%v err=%v, want tool_old", e2.ToolUseID, ok, err)
	}
}

// restore @1 and restore <fullkey> produce the same result.
func TestRestoreAtNEquivalentToKey(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "original")
	f.capture(t, "tool_eq", "x", func() { f.write(t, "f.txt", "changed") })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"@1"}, &out, &errb); code != 0 {
		t.Fatalf("restore @1 exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(f.work, "f.txt")); string(b) != "original" {
		t.Fatalf("restore @1 did not revert: %q", b)
	}
}

func TestAtNOutOfRange(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_a", "x", func() { f.write(t, "f.txt", "v1") })

	if _, _, err := resolveEntry(f.layout, f.work, "@5"); err == nil {
		t.Fatal("@5 out of range should error")
	}
	if _, _, err := resolveEntry(f.layout, f.work, "@0"); err == nil {
		t.Fatal("@0 should be invalid")
	}
	if _, _, err := resolveEntry(f.layout, f.work, "@abc"); err == nil {
		t.Fatal("@abc should be invalid")
	}
}

// An out-of-range @N from a user command surfaces the error clearly.
func TestRestoreAtNOutOfRangeMessage(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"@9"}, &out, &errb); code == 0 {
		t.Fatal("@9 with no entries should fail")
	}
	if !bytes.Contains(errb.Bytes(), []byte("out of range")) {
		t.Fatalf("want out-of-range message, got %q", errb.String())
	}
}

// Full keys still match exactly, and a unique prefix resolves.
func TestKeyExactAndPrefix(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_unique_abc", "x", func() { f.write(t, "f.txt", "v1") })

	if _, ok, err := resolveEntry(f.layout, f.work, "tool_unique_abc"); !ok || err != nil {
		t.Fatalf("exact key: ok=%v err=%v", ok, err)
	}
	if e, ok, err := resolveEntry(f.layout, f.work, "tool_uniq"); !ok || err != nil || e.ToolUseID != "tool_unique_abc" {
		t.Fatalf("unique prefix: e=%q ok=%v err=%v", e.ToolUseID, ok, err)
	}
}

// A prefix shorter than the 4-char floor is refused even when unique, so a stray
// 1-3 char fragment never silently resolves to a surprising entry.
func TestResolvePrefixTooShort(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_abc123", "x", func() { f.write(t, "f.txt", "v1") })

	_, _, err := resolveEntry(f.layout, f.work, "too")
	if err == nil {
		t.Fatal("3-char prefix should be refused as too short")
	}
	if !strings.Contains(err.Error(), "too short (use at least 4 characters") {
		t.Fatalf("want too-short message, got %q", err.Error())
	}
}

// The 4-char floor is inclusive: a 4-char unique prefix resolves.
func TestResolvePrefixFourChars(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "wxyz_unique", "x", func() { f.write(t, "f.txt", "v1") })

	e, ok, err := resolveEntry(f.layout, f.work, "wxyz")
	if err != nil || !ok || e.ToolUseID != "wxyz_unique" {
		t.Fatalf("4-char prefix: e=%q ok=%v err=%v", e.ToolUseID, ok, err)
	}
}

// Every key-addressing subcommand resolves an unambiguous prefix identically;
// pins the shared resolveEntry path so one subcommand can't silently diverge.
func TestResolvePrefixPerSubcommand(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_matrix_abc", "echo hi", func() { f.write(t, "f.txt", "v1") })

	const prefix = "toolu_m" // 7 chars, the sole entry — unambiguous

	cases := []struct {
		name string
		args []string
		fn   func(paths.Layout, string, []string, io.Writer, io.Writer) int
	}{
		{"diff", []string{prefix}, Diff},
		{"show", []string{prefix}, Show},
		{"restore --dry-run", []string{prefix, "--dry-run"}, Restore},
		{"export", []string{prefix}, Export},
		{"rewind --dry-run", []string{prefix, "--dry-run"}, Rewind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := tc.fn(f.layout, f.work, tc.args, &out, &errb); code != 0 {
				t.Fatalf("%s with prefix %q exit %d: %s", tc.name, prefix, code, errb.String())
			}
		})
	}
}

func TestAmbiguousPrefix(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_dup_1", "x", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_dup_2", "x", func() { f.write(t, "f.txt", "v2") })

	if _, _, err := resolveEntry(f.layout, f.work, "tool_dup"); err == nil {
		t.Fatal("ambiguous prefix should error")
	}
}

// list --json exposes the @N index alongside the full key.
func TestListJSONIndex(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_i1", "x", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_i2", "x", func() { f.write(t, "f.txt", "v2") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	entries := m["entries"].([]any)
	for _, raw := range entries {
		e := raw.(map[string]any)
		if e["key"] == "tool_i2" && int(e["index"].(float64)) != 1 {
			t.Fatalf("newest tool_i2 should be @1, got %v", e["index"])
		}
		if e["key"] == "tool_i1" && int(e["index"].(float64)) != 2 {
			t.Fatalf("older tool_i1 should be @2, got %v", e["index"])
		}
	}
}
