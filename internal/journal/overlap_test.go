package journal

import "testing"

func TestMarkOverlapsCrossSession(t *testing.T) {
	entries := []Entry{
		// sA: [00:00:00, 00:00:05]
		{ToolUseID: "a", SessionID: "sA", TS: "2026-06-10T00:00:05Z", DurationMS: 5000, PostSHA: "pa"},
		// sB: [00:00:03, 00:00:08] -> intersects sA
		{ToolUseID: "b", SessionID: "sB", TS: "2026-06-10T00:00:08Z", DurationMS: 5000, PostSHA: "pb"},
		// sC: [00:00:15, 00:00:20] -> disjoint
		{ToolUseID: "c", SessionID: "sC", TS: "2026-06-10T00:00:20Z", DurationMS: 5000, PostSHA: "pc"},
	}
	got := MarkOverlaps(entries)
	if !got[0].Overlapped || !got[1].Overlapped {
		t.Fatalf("a and b should be cross-session overlapped: a=%v b=%v", got[0].Overlapped, got[1].Overlapped)
	}
	if got[2].Overlapped {
		t.Fatalf("c is disjoint and must not be overlapped")
	}
}

func TestMarkOverlapsTouchingNotMarked(t *testing.T) {
	entries := []Entry{
		{ToolUseID: "a", SessionID: "sA", TS: "2026-06-10T00:00:05Z", DurationMS: 5000, PostSHA: "pa"},
		// [00:00:05, 00:00:10] -> shares only the endpoint with sA
		{ToolUseID: "b", SessionID: "sB", TS: "2026-06-10T00:00:10Z", DurationMS: 5000, PostSHA: "pb"},
	}
	got := MarkOverlaps(entries)
	if got[0].Overlapped || got[1].Overlapped {
		t.Fatalf("exactly-touching intervals must not overlap: %v %v", got[0].Overlapped, got[1].Overlapped)
	}
}

func TestMarkOverlapsSameSessionNotComputed(t *testing.T) {
	entries := []Entry{
		{ToolUseID: "a", SessionID: "s", TS: "2026-06-10T00:00:05Z", DurationMS: 5000, PostSHA: "pa"},
		{ToolUseID: "b", SessionID: "s", TS: "2026-06-10T00:00:08Z", DurationMS: 5000, PostSHA: "pb"},
	}
	got := MarkOverlaps(entries)
	if got[0].Overlapped || got[1].Overlapped {
		t.Fatalf("same-session overlap is the daemon's job, not the read-time computation")
	}
}

func TestMarkOverlapsPreservesStoredFlag(t *testing.T) {
	entries := []Entry{
		{ToolUseID: "a", SessionID: "s", TS: "2026-06-10T00:00:05Z", DurationMS: 5000, PostSHA: "pa", Overlapped: true},
	}
	if got := MarkOverlaps(entries); !got[0].Overlapped {
		t.Fatal("stored overlap flag must not be cleared")
	}
}

func TestMarkOverlapsSkipsPreOnly(t *testing.T) {
	entries := []Entry{
		// pre-only: no post snapshot, must be skipped even if ts overlaps.
		{ToolUseID: "a", SessionID: "sA", TS: "2026-06-10T00:00:05Z", PreSHA: "pre"},
		{ToolUseID: "b", SessionID: "sB", TS: "2026-06-10T00:00:05Z", DurationMS: 5000, PostSHA: "pb"},
	}
	got := MarkOverlaps(entries)
	if got[0].Overlapped || got[1].Overlapped {
		t.Fatalf("pre-only entries are skipped by overlap computation: %v %v", got[0].Overlapped, got[1].Overlapped)
	}
}
