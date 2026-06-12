package journal

import (
	"os"
	"path/filepath"
	"testing"
)

// bg_task_id round-trips through append/read and merges from whichever half-row
// carries it (later non-empty wins), like the other additive post fields.
func TestBgTaskIDRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, Append(p, Entry{ToolUseID: "toolu_bg", PreSHA: "pre1", TS: "2026-06-10T00:00:00Z"}))
	must(t, Append(p, Entry{ToolUseID: "toolu_bg", PostSHA: "post1", Status: StatusProtected,
		BgTaskID: "b32gd3xrm"}))

	merged, err := ReadMerged(p, ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || merged[0].BgTaskID != "b32gd3xrm" {
		t.Fatalf("merge lost bg_task_id: %+v", merged)
	}
}

// A row written before bg_task_id existed reads back cleanly with an empty field.
func TestOldRowWithoutBgTaskID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, os.WriteFile(p, []byte(`{"v":1,"tool_use_id":"toolu_old","status":"protected","pre_sha":"a","post_sha":"b"}`+"\n"), 0o600))
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].BgTaskID != "" {
		t.Fatalf("old row should have no bg_task_id: %+v", got[0])
	}
}
