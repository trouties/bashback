package cli

import (
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

func tsAgo(now time.Time, d time.Duration) string {
	return now.Add(-d).UTC().Format(time.RFC3339)
}

// Only sessions with a non-manual entry inside the window count, newest-activity first.
func TestActiveSessionsWindow(t *testing.T) {
	now := time.Now().UTC()
	entries := []journal.Entry{
		{SessionID: "s1", TS: tsAgo(now, 10*time.Minute), Command: "echo one", PostSHA: "a"},
		{SessionID: "s2", TS: tsAgo(now, 50*time.Minute), Command: "echo two", PostSHA: "b"},
		{SessionID: "s3", TS: tsAgo(now, 2*time.Hour), Command: "echo three", PostSHA: "c"},
	}
	got := activeSessions(entries, now)
	if len(got) != 2 {
		t.Fatalf("active sessions = %d (%+v), want 2", len(got), got)
	}
	if got[0].ID != "s1" || got[1].ID != "s2" {
		t.Fatalf("order = [%s %s], want [s1 s2]", got[0].ID, got[1].ID)
	}
	if got[0].LastCommand != "echo one" {
		t.Fatalf("s1 last command = %q, want %q", got[0].LastCommand, "echo one")
	}
}

// Manual entries don't count toward activity, so a session whose only recent entry is manual is inactive.
func TestActiveSessionsExcludesManual(t *testing.T) {
	now := time.Now().UTC()
	entries := []journal.Entry{
		{SessionID: "s1", TS: tsAgo(now, 5*time.Minute), Command: "snap", PreSHA: "x", Status: journal.StatusManual},
		{SessionID: "s1", TS: tsAgo(now, 2*time.Hour), Command: "echo old", PostSHA: "y"},
		{SessionID: "s2", TS: tsAgo(now, 10*time.Minute), Command: "echo two", PostSHA: "z"},
	}
	got := activeSessions(entries, now)
	if len(got) != 1 || got[0].ID != "s2" {
		t.Fatalf("active = %+v, want only s2", got)
	}
}

func TestShortSessionID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1f2ce00a-1234-5678-9abc-def012345678", "1f2ce00a"},
		{"short", "short"},
		{"exactly8", "exactly8"},
	}
	for _, c := range cases {
		if got := shortSession(c.in); got != c.want {
			t.Errorf("shortSession(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
