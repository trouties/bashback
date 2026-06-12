package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAppendAndRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	e := Entry{ToolUseID: "toolu_1", SessionID: "s", TS: "2026-06-10T00:00:00Z", Command: "rm -rf x", PreSHA: "aaa", PostSHA: "bbb", Status: StatusProtected, DurationMS: 95}
	if err := Append(p, e); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].V != SchemaVersion || got[0].ToolUseID != "toolu_1" || got[0].Status != StatusProtected {
		t.Fatalf("read = %+v", got)
	}
}

func TestReadRejectsFutureVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := os.WriteFile(p, []byte(`{"v":999,"tool_use_id":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil {
		t.Fatal("future schema version must error")
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing journal: got %v err %v", got, err)
	}
}

// Degraded direct-write path writes a pre half-row then a post half-row; the
// read side must merge them into one logical entry keyed by tool_use_id.
func TestHalfRowMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, Append(p, Entry{ToolUseID: "toolu_x", Command: "sed -i s/a/b/ f", PreSHA: "pre1", TS: "2026-06-10T00:00:00Z"}))
	must(t, Append(p, Entry{ToolUseID: "toolu_x", PostSHA: "post1", Status: StatusProtected, DurationMS: 42}))

	merged, err := ReadMerged(p, ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 {
		t.Fatalf("want 1 merged entry, got %d", len(merged))
	}
	m := merged[0]
	if m.PreSHA != "pre1" || m.PostSHA != "post1" || m.Status != StatusProtected || m.Command != "sed -i s/a/b/ f" || m.DurationMS != 42 {
		t.Fatalf("merge lost fields: %+v", m)
	}
}

func TestRestoredEntryDoesNotMergeWithOriginal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, Append(p, Entry{ToolUseID: "toolu_x", Status: StatusProtected, PreSHA: "a", PostSHA: "b"}))
	must(t, Append(p, Entry{ToolUseID: "restore_b0", Status: StatusRestored, PreSHA: "b", PostSHA: "a"}))
	merged, err := ReadMerged(p, ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 2 {
		t.Fatalf("restored entry must stay separate, got %d", len(merged))
	}
}

func TestCompositeKeyer(t *testing.T) {
	k := CompositeKeyer{}
	a := Entry{SessionID: "s1", Seq: 3, CmdHash: "h"}
	b := Entry{SessionID: "s1", Seq: 3, CmdHash: "h"}
	c := Entry{SessionID: "s1", Seq: 4, CmdHash: "h"}
	if k.Key(a) != k.Key(b) {
		t.Fatal("identical composite keys should match")
	}
	if k.Key(a) == k.Key(c) {
		t.Fatal("different seq should differ")
	}
}

func TestConcurrentAppendNoInterleave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			must(t, Append(p, Entry{ToolUseID: "id", Command: "c", Seq: i, Status: StatusProtected}))
		}(i)
	}
	wg.Wait()

	// Every line must be valid JSON (no torn writes) and the count must match.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range splitNonEmpty(string(b)) {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("torn/interleaved line: %q: %v", line, err)
		}
		lines++
	}
	if lines != n {
		t.Fatalf("want %d lines, got %d", n, lines)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestOriginRoundTripAndMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	if err := Append(p, Entry{ToolUseID: "t1", Origin: "cursor", PreSHA: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, Entry{ToolUseID: "t1", PostSHA: "b"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMerged(p, DefaultKeyer)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v err %v", got, err)
	}
	if got[0].Origin != "cursor" {
		t.Fatalf("origin lost in merge: %q", got[0].Origin)
	}
}

func TestOriginAbsentIsBackwardCompatible(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	if err := os.WriteFile(p, []byte(`{"v":1,"tool_use_id":"old","pre_sha":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil || len(got) != 1 || got[0].Origin != "" {
		t.Fatalf("old row must read with empty origin: %v err %v", got, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
