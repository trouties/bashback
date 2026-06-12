package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// bgOf recovers the original command's key from a bgfinal_ marker, empty otherwise.
func TestBgOfHelper(t *testing.T) {
	if got := bgOf("bgfinal_toolu_x9"); got != "toolu_x9" {
		t.Fatalf("bgOf(bgfinal_toolu_x9) = %q, want toolu_x9", got)
	}
	if got := bgOf("toolu_x9"); got != "" {
		t.Fatalf("bgOf(non-bgfinal) = %q, want empty", got)
	}
}

// A bgfinal row is attributed to its original via a "(bg of <short key>)" command prefix.
func TestLogBgfinalAttribution(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_x9", "echo orig", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "bgfinal_toolu_x9", "echo bgfinal", func() { f.write(t, "f.txt", "v2") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "(bg of toolu_x9)") {
		t.Fatalf("missing bgfinal attribution:\n%s", out.String())
	}
}

// --json carries the full original key in bg_of for bgfinal rows, omitting it otherwise.
func TestLogJSONBgOf(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_x9", "echo orig", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "bgfinal_toolu_x9", "echo bgfinal", func() { f.write(t, "f.txt", "v2") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt", "--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	entries := m["entries"].([]any)
	for _, raw := range entries {
		e := raw.(map[string]any)
		switch e["key"] {
		case "bgfinal_toolu_x9":
			if e["bg_of"] != "toolu_x9" {
				t.Fatalf("bgfinal row bg_of = %v, want full key toolu_x9", e["bg_of"])
			}
		case "toolu_x9":
			if _, has := e["bg_of"]; has {
				t.Fatalf("ordinary row should omit bg_of, got %v", e["bg_of"])
			}
		}
	}
}

// log's KEY column shows shortest-unique keys, extending past a shared prefix on collision.
func TestLogShortKeys(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_aaaaaaaabbbbbbbb_one", "echo a", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "toolu_aaaaaaaabbbbbbbb_two", "echo b", func() { f.write(t, "f.txt", "v2") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	keys := keyColumn(t, out.String())
	if len(keys) != 2 {
		t.Fatalf("expected 2 KEY values, got %v", keys)
	}
	if keys[0] == keys[1] {
		t.Fatalf("colliding keys not disambiguated: both %q", keys[0])
	}
	for _, k := range keys {
		if strings.Contains(k, "…") {
			t.Fatalf("KEY column shows an ellipsis: %q", k)
		}
		var so, se bytes.Buffer
		if code := Show(f.layout, f.work, []string{k}, &so, &se); code != 0 {
			t.Fatalf("show %q failed: %s", k, se.String())
		}
	}
}

// -n keeps only the latest N matching entries, like list.
func TestLogLimitN(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_1", "c1", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_2", "c2", func() { f.write(t, "f.txt", "v2") })
	f.capture(t, "tool_3", "c3", func() { f.write(t, "f.txt", "v3") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt", "-n", "1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "tool_3") || strings.Contains(s, "tool_1") || strings.Contains(s, "tool_2") {
		t.Fatalf("-n 1 should show only the newest (tool_3): %q", s)
	}
}

// --since accepts both a duration and an RFC3339 timestamp, with list semantics.
func TestLogSince(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_old", "old", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "tool_new", "new", func() { f.write(t, "f.txt", "v2") })

	now := time.Now()
	jp := f.layout.JournalPath(f.work)
	entries, _ := journal.ReadMerged(jp, journal.DefaultKeyer)
	if err := os.Remove(jp); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.ToolUseID {
		case "tool_old":
			e.TS = now.Add(-90 * time.Minute).Format(time.RFC3339)
		case "tool_new":
			e.TS = now.Add(-30 * time.Minute).Format(time.RFC3339)
		}
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}

	for _, since := range []string{"1h", now.Add(-45 * time.Minute).Format(time.RFC3339)} {
		var out, errb bytes.Buffer
		if code := Log(f.layout, f.work, []string{"f.txt", "--since", since}, &out, &errb); code != 0 {
			t.Fatalf("--since %s exit %d: %s", since, code, errb.String())
		}
		if !strings.Contains(out.String(), "tool_new") || strings.Contains(out.String(), "tool_old") {
			t.Fatalf("--since %s should keep tool_new only: %q", since, out.String())
		}
	}
}

// --full leaves commands untruncated; --abs shows raw RFC3339 timestamps.
func TestLogFullAndAbs(t *testing.T) {
	f := newFix(t)
	longCmd := "echo " + strings.Repeat("a", 120)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_fa", longCmd, func() { f.write(t, "f.txt", "v1") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt", "--full"}, &out, &errb); code != 0 {
		t.Fatalf("--full exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), longCmd) {
		t.Fatalf("--full should not truncate the command: %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Log(f.layout, f.work, []string{"f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if strings.Contains(out.String(), longCmd) {
		t.Fatalf("default should truncate a long command: %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Log(f.layout, f.work, []string{"f.txt", "--abs"}, &out, &errb); code != 0 {
		t.Fatalf("--abs exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "2026-06-10T") {
		t.Fatalf("--abs should show raw RFC3339 ts: %q", out.String())
	}
}

// -h/--help prints usage to stderr, never falling into the path-argument path
// that would report "no recorded changes to -h".
func TestLogHelpFlag(t *testing.T) {
	f := newFix(t)
	for _, h := range []string{"-h", "--help"} {
		var out, errb bytes.Buffer
		code := Log(f.layout, f.work, []string{h}, &out, &errb)
		if code != 0 {
			t.Errorf("log %s exit = %d, want 0", h, code)
		}
		if !strings.Contains(out.String(), "usage") {
			t.Errorf("log %s should print usage to stdout, got %q", h, out.String())
		}
		if strings.Contains(out.String()+errb.String(), "no recorded changes") {
			t.Errorf("log %s fell into the positional path: %q", h, out.String()+errb.String())
		}
	}
}

func TestLogExactAndPrefix(t *testing.T) {
	f := newFix(t)
	f.write(t, "src/a.go", "v0")
	f.write(t, "other.txt", "x")
	f.capture(t, "tool_1", "edit a", func() { f.write(t, "src/a.go", "v1") })
	f.capture(t, "tool_2", "edit other", func() { f.write(t, "other.txt", "y") })
	f.capture(t, "tool_3", "add b", func() { f.write(t, "src/b.go", "new") })

	// Exact file match.
	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"src/a.go"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "tool_1") || strings.Contains(s, "tool_2") {
		t.Fatalf("exact match: %q", s)
	}

	// Directory prefix matches both src files.
	out.Reset()
	errb.Reset()
	if code := Log(f.layout, f.work, []string{"src"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	s = out.String()
	if !strings.Contains(s, "tool_1") || !strings.Contains(s, "tool_3") || strings.Contains(s, "tool_2") {
		t.Fatalf("dir prefix match: %q", s)
	}
}

// log is pure-journal: it still works after the snapshots are reclaimed.
func TestLogAfterReclaim(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_r", "x", func() { f.write(t, "f.txt", "v1") })
	os.RemoveAll(f.layout.SessionGitDir(f.work, f.session)) // reclaim the shadow repo

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt"}, &out, &errb); code != 0 {
		t.Fatalf("log after reclaim exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tool_r") {
		t.Fatalf("log should work from the journal alone: %q", out.String())
	}
}

// Old entries without the files field are counted in a trailing note.
func TestLogOlderWithoutData(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_new", "x", func() { f.write(t, "f.txt", "v1") })
	// Inject an old protected entry with a real change but no files field.
	journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "tool_legacy", SessionID: "sess1", TS: "2026-06-09T00:00:00Z",
		Command: "legacy", PreSHA: "a", PostSHA: "b", Status: journal.StatusProtected,
	})

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "no file data") {
		t.Fatalf("should note older entries without file data: %q", out.String())
	}
}

func TestLogJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_j", "x", func() { f.write(t, "f.txt", "v1") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"--json", "f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if m["path"] != "f.txt" {
		t.Fatalf("path = %v", m["path"])
	}
	entries, _ := m["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", m["entries"])
	}
	e0 := entries[0].(map[string]any)
	if e0["change"] != "M" {
		t.Fatalf("change = %v, want M", e0["change"])
	}
}

func TestLogJSONCarriesOrigin(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "0")
	f.capture(t, "tool_origin", "echo hi", func() { f.write(t, "a.txt", "1") })
	// Tag the captured entry's origin by appending a merge fragment.
	jp := f.layout.JournalPath(f.work)
	if err := journal.Append(jp, journal.Entry{ToolUseID: "tool_origin", Origin: "cursor"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"--json", "a.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), `"origin": "cursor"`) {
		t.Errorf("log --json missing origin: %s", out.String())
	}
}

// log --json is newest-first: the latest touch is entries[0].
func TestLogJSONNewestFirst(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "0")
	f.capture(t, "tool_l1", "first", func() { f.write(t, "f.txt", "1") })
	f.capture(t, "tool_l2", "second", func() { f.write(t, "f.txt", "2") })

	var out, errb bytes.Buffer
	if code := Log(f.layout, f.work, []string{"--json", "f.txt"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	entries, _ := m["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %v", m["entries"])
	}
	if entries[0].(map[string]any)["key"] != "tool_l2" {
		t.Fatalf("entries[0] should be newest tool_l2, got %v", entries[0])
	}
}
