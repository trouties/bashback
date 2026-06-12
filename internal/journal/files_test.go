package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilesRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	e := Entry{
		ToolUseID:    "toolu_1",
		PreSHA:       "a",
		PostSHA:      "b",
		Status:       StatusProtected,
		Files:        []FileChange{{P: "x.txt", S: "M"}, {P: "y.txt", S: "A"}},
		FilesOmitted: 3,
	}
	must(t, Append(p, e))
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].V != SchemaVersion {
		t.Fatalf("read = %+v", got)
	}
	if len(got[0].Files) != 2 || got[0].Files[0].P != "x.txt" || got[0].Files[0].S != "M" {
		t.Fatalf("files lost: %+v", got[0].Files)
	}
	if got[0].FilesOmitted != 3 {
		t.Fatalf("files_omitted = %d, want 3", got[0].FilesOmitted)
	}
}

// An old row written before the files field must read back cleanly with v=1 and
// nil Files (no schema bump).
func TestOldRowWithoutFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, os.WriteFile(p, []byte(`{"v":1,"tool_use_id":"toolu_old","status":"protected","pre_sha":"a","post_sha":"b"}`+"\n"), 0o600))
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Files != nil || got[0].FilesOmitted != 0 {
		t.Fatalf("old row should have no files: %+v", got[0])
	}
}

// The post half-row carries files; the merged view must keep them.
func TestFilesMergeFromPostHalfRow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.jsonl")
	must(t, Append(p, Entry{ToolUseID: "toolu_x", PreSHA: "pre1", TS: "2026-06-10T00:00:00Z"}))
	must(t, Append(p, Entry{ToolUseID: "toolu_x", PostSHA: "post1", Status: StatusProtected,
		Files: []FileChange{{P: "f.txt", S: "M"}}, FilesOmitted: 0}))
	merged, err := ReadMerged(p, ToolUseIDKeyer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || len(merged[0].Files) != 1 || merged[0].Files[0].P != "f.txt" {
		t.Fatalf("merge lost files: %+v", merged)
	}
}
