package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

func TestListJSONCarriesOrigin(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	jp := f.layout.JournalPath(f.work)
	must := func(e journal.Entry) {
		if err := journal.Append(jp, e); err != nil {
			t.Fatal(err)
		}
	}
	must(journal.Entry{ToolUseID: "tool_origin", SessionID: f.session, TS: f.nextTS(), Status: journal.StatusProtected, PreSHA: "a", PostSHA: "b", Origin: "cursor"})
	must(journal.Entry{ToolUseID: "tool_plain", SessionID: f.session, TS: f.nextTS(), Status: journal.StatusProtected, PreSHA: "c", PostSHA: "d"})

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), `"origin": "cursor"`) {
		t.Errorf("list --json missing origin: %s", out.String())
	}
	if n := strings.Count(out.String(), `"origin"`); n != 1 {
		t.Errorf("origin emitted %d times, want 1 (omitempty for blank): %s", n, out.String())
	}
}

func TestListStatusAnnotatesOrigin(t *testing.T) {
	f := newFix(t)
	rec := newReclaimedMemo(f.layout, f.work)
	rec.seen["codexsess"] = false
	e := journal.Entry{Status: journal.StatusProtected, SessionID: "codexsess", Origin: "codex", PreSHA: "a", PostSHA: "b"}
	got := listStatus(rec, e)
	if !strings.Contains(got, "(codex)") {
		t.Errorf("listStatus = %q, want origin annotation", got)
	}
}

// keyColumn returns the KEY column (field 1, after @N) of each data row, skipping the header.
func keyColumn(t *testing.T, table string) []string {
	t.Helper()
	var keys []string
	lines := strings.Split(strings.TrimSpace(table), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "@") {
			continue
		}
		keys = append(keys, fields[1])
	}
	return keys
}

// The KEY column shows shortest-unique keys that feed straight back into show
// ("displayed is usable") and never carry an ellipsis.
func TestListShortKeysResolvable(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "toolu_aaaaaaaabbbbbbbb_one", "echo a", func() { f.write(t, "f.txt", "v1") })
	f.capture(t, "bgfinal_toolu_ccccccccdddddddd_two", "echo b", func() { f.write(t, "f.txt", "v2") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	keys := keyColumn(t, out.String())
	if len(keys) != 2 {
		t.Fatalf("expected 2 KEY values, got %v", keys)
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

// --full restores the full key in the KEY column.
func TestListFullShowsFullKey(t *testing.T) {
	f := newFix(t)
	const full = "toolu_aaaaaaaabbbbbbbb_one"
	f.write(t, "f.txt", "v0")
	f.capture(t, full, "echo a", func() { f.write(t, "f.txt", "v1") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--full"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), full) {
		t.Fatalf("--full should show the full key %q, got:\n%s", full, out.String())
	}
}

func TestReadCommandsHandleReclaimedSnapshots(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	key := f.capture(t, "tool_recl", "echo a", func() { f.write(t, "f.txt", "v2") })
	if err := os.RemoveAll(f.layout.SessionGitDir(f.work, f.session)); err != nil {
		t.Fatal(err)
	}

	var lo, le bytes.Buffer
	if code := List(f.layout, f.work, nil, &lo, &le); code != 0 {
		t.Fatalf("list exit: %s", le.String())
	}
	if !strings.Contains(lo.String(), "reclaimed") {
		t.Errorf("list should flag reclaimed:\n%s", lo.String())
	}

	var do, de bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key}, &do, &de); code == 0 {
		t.Errorf("diff on a reclaimed key should be non-zero")
	}
	if strings.Contains(de.String(), "bad object") {
		t.Errorf("raw git error leaked from diff: %s", de.String())
	}
	if !strings.Contains(de.String(), "reclaimed") {
		t.Errorf("diff should explain reclaimed: %s", de.String())
	}
}

func TestDiffPatchOverLimitFallsBackToStat(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "seed")
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "line %d with some padding content here\n", i)
	}
	key := f.capture(t, "tool_big", "generate", func() { f.write(t, "f.txt", sb.String()) })

	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("diff exit: %s", errb.String())
	}
	s := out.String()
	if strings.Contains(s, "diff --git") {
		t.Errorf("over-limit text diff should not dump the raw patch")
	}
	if !strings.Contains(s, "display limit") {
		t.Errorf("missing over-limit notice:\n%s", s)
	}
	if !strings.Contains(s, "export") || !strings.Contains(s, "bashback diff") {
		t.Errorf("missing escape-hatch hints:\n%s", s)
	}

	var jo, je bytes.Buffer
	if code := Diff(f.layout, f.work, []string{"--json", key}, &jo, &je); code != 0 {
		t.Fatalf("diff --json exit: %s", je.String())
	}
	if !strings.Contains(jo.String(), "diff --git") {
		t.Error("--json must carry the full patch")
	}
	if !strings.Contains(jo.String(), `"patch_bytes"`) {
		t.Error("--json must report patch_bytes")
	}
}

func TestDiffManualPointsAtRewind(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	key := f.preCapture(t, "snap_manual1", "snap", func() { f.write(t, "f.txt", "v1") })
	if err := journal.Append(f.layout.JournalPath(f.work), journal.Entry{ToolUseID: key, Status: journal.StatusManual}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	Diff(f.layout, f.work, []string{key}, &out, &errb)
	s := errb.String()
	if !strings.Contains(s, "bashback rewind") {
		t.Errorf("manual checkpoint diff should point at rewind: %s", s)
	}
	if strings.Contains(s, "restore") && strings.Contains(s, "--force") {
		t.Errorf("manual guidance should not mention restore --force: %s", s)
	}
}

func TestListEmptyStateGuidesToDoctor(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "bashback doctor") {
		t.Fatalf("empty-state output should point at doctor: %q", out.String())
	}
}

func TestListEmptyStateMentionsProtectedParent(t *testing.T) {
	f := newFix(t)
	// The parent project is protected (has a meta.json); the child subdir is not.
	parent := f.work
	if err := f.layout.EnsureRepoDirs(parent); err != nil {
		t.Fatal(err)
	}
	if err := f.layout.WriteMeta(parent, paths.Meta{OriginalPath: parent}); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "sub")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := List(f.layout, child, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), parent) {
		t.Fatalf("subdir empty-state should mention the protected parent %q: %q", parent, out.String())
	}
}

func TestListDefaultWindowBoundsTextButNotJSON(t *testing.T) {
	f := newFix(t)
	jp := f.layout.JournalPath(f.work)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := journal.Append(jp, journal.Entry{
			ToolUseID: fmt.Sprintf("tool%d", i), SessionID: "sess1", TS: f.nextTS(),
			Command: fmt.Sprintf("cmd%d", i), PreSHA: fmt.Sprintf("p%d", i), PostSHA: fmt.Sprintf("q%d", i),
			Status: journal.StatusProtected,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "(+10 older entries; use -n 0 for all)") {
		t.Fatalf("default window should note the elided 10: %q", out.String())
	}

	var jout, jerr bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &jout, &jerr); code != 0 {
		t.Fatalf("list --json exit %d: %s", code, jerr.String())
	}
	if got := strings.Count(jout.String(), `"key"`); got != 60 {
		t.Fatalf("--json returned %d entries, want all 60", got)
	}

	var aout, aerr bytes.Buffer
	if code := List(f.layout, f.work, []string{"-n", "0"}, &aout, &aerr); code != 0 {
		t.Fatalf("list -n 0 exit %d: %s", code, aerr.String())
	}
	if strings.Contains(aout.String(), "older entries") {
		t.Fatalf("-n 0 should show all entries, not a window note: %q", aout.String())
	}
}

func TestDiffNoEntryGuidesToList(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	Diff(f.layout, f.work, []string{"nope"}, &out, &errb)
	if !strings.Contains(errb.String(), "bashback list") {
		t.Fatalf("no-entry error should point at list: %q", errb.String())
	}
}
