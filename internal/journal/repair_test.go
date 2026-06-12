package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadToleratesTornTailLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	good := `{"v":1,"tool_use_id":"a","pre_sha":"x"}` + "\n"
	if err := os.WriteFile(p, []byte(good+`{"v":1,"tool_use_`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

func TestReadStillRejectsMidFileCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	content := `{"v":1,"tool_use_` + "\n" + `{"v":1,"tool_use_id":"b"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil {
		t.Fatal("mid-file corruption accepted, want error")
	}
}

func TestRepairQuarantinesBadLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	bad := `{"v":1,"tool_use_`
	content := `{"v":1,"tool_use_id":"a"}` + "\n" + bad + "\n" + `{"v":1,"tool_use_id":"c"}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := Repair(p)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	entries, err := Read(p)
	if err != nil {
		t.Fatalf("Read after repair: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	b, err := os.ReadFile(p + ".bad")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), bad) {
		t.Fatalf("journal.bad = %q, want it to hold the bad line verbatim", string(b))
	}
}

func TestRepairCleanJournalIsNoOp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	if err := os.WriteFile(p, []byte(`{"v":1,"tool_use_id":"a"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := Repair(p)
	if err != nil || moved != 0 {
		t.Fatalf("Repair clean journal = (%d, %v), want (0, nil)", moved, err)
	}
}
