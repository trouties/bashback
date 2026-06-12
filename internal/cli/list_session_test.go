package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// dataRowCount counts rendered entry rows (`@<n>`), excluding the `@N` header.
func dataRowCount(s string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if len(ln) >= 2 && ln[0] == '@' && ln[1] >= '0' && ln[1] <= '9' {
			n++
		}
	}
	return n
}

// Interleaved sessions render as per-session groups with a count header, rows in file order.
func TestListBySessionGroups(t *testing.T) {
	f := newFix(t)
	f.write(t, "a", "1")
	f.capture(t, "tool_s1a", "echo s1a", func() { f.write(t, "a", "2") })
	f.session = "sess2"
	f.capture(t, "tool_s2a", "echo s2a", func() { f.write(t, "b", "1") })
	f.session = "sess1"
	f.capture(t, "tool_s1b", "echo s1b", func() { f.write(t, "a", "3") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--by-session"}, &out, &errb); code != 0 {
		t.Fatalf("list --by-session exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "session sess1") || !strings.Contains(s, "session sess2") {
		t.Fatalf("missing group headers: %q", s)
	}
	if !strings.Contains(s, "2 entries") || !strings.Contains(s, "1 entry") {
		t.Fatalf("missing group counts: %q", s)
	}
	for _, id := range []string{"tool_s1a", "tool_s1b", "tool_s2a"} {
		if !strings.Contains(s, id) {
			t.Fatalf("missing entry %q in %q", id, s)
		}
	}
	// sess1 group keeps file order: s1a before s1b.
	if strings.Index(s, "tool_s1a") > strings.Index(s, "tool_s1b") {
		t.Fatalf("group rows should stay in file order: %q", s)
	}
}

// An all-manual session group is tagged [manual].
func TestListBySessionManualGroup(t *testing.T) {
	f := newFix(t)
	f.write(t, "a", "1")
	f.capture(t, "tool_real", "echo real", func() { f.write(t, "a", "2") })
	if err := journal.Append(f.layout.JournalPath(f.work), journal.Entry{
		ToolUseID: "snap1", SessionID: "snapsess", TS: f.nextTS(),
		Command: "snap", PreSHA: "deadbeef", Status: journal.StatusManual,
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--by-session"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "session snapsess") || !strings.Contains(s, "[manual]") {
		t.Fatalf("manual group should be tagged [manual]: %q", s)
	}
	// The real session group must not carry the manual tag.
	realHeader := s[strings.Index(s, "session sess1"):]
	realHeader = realHeader[:strings.IndexByte(realHeader, '\n')]
	if strings.Contains(realHeader, "[manual]") {
		t.Fatalf("non-manual group wrongly tagged: %q", realHeader)
	}
}

// A session with a live socket listener is tagged [live].
func TestListBySessionLive(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	f.write(t, "a", "1")
	f.capture(t, "tool_live", "echo live", func() { f.write(t, "a", "2") })

	ln, err := net.Listen("unix", f.layout.SocketPath("sess1"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--by-session"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "[live]") {
		t.Fatalf("live session should be tagged [live]: %q", out.String())
	}
}

// Filters apply before grouping, so the grouped view shows exactly the flat-filtered rows.
func TestListBySessionWithFilters(t *testing.T) {
	f := newFix(t)
	f.write(t, "a", "1")
	f.capture(t, "tool_s1a", "echo s1a", func() { f.write(t, "a", "2") })
	f.session = "sess2"
	f.capture(t, "tool_s2a", "echo s2a", func() { f.write(t, "b", "1") })
	f.session = "sess1"
	f.capture(t, "tool_s1b", "echo s1b", func() { f.write(t, "a", "3") })
	now := time.Now().UTC()
	retimeJournal(t, f, map[string]string{
		"tool_s1a": tsAgo(now, 30*time.Minute),
		"tool_s2a": tsAgo(now, 20*time.Minute),
		"tool_s1b": tsAgo(now, 10*time.Minute),
	})

	args := []string{"--since", "1h", "-n", "2"}
	var flat, errb bytes.Buffer
	if code := List(f.layout, f.work, args, &flat, &errb); code != 0 {
		t.Fatalf("flat list exit %d: %s", code, errb.String())
	}
	var grouped bytes.Buffer
	errb.Reset()
	if code := List(f.layout, f.work, append([]string{"--by-session"}, args...), &grouped, &errb); code != 0 {
		t.Fatalf("grouped list exit %d: %s", code, errb.String())
	}
	if got, want := dataRowCount(grouped.String()), dataRowCount(flat.String()); got != want {
		t.Fatalf("grouped row count %d != flat %d\nflat:\n%s\ngrouped:\n%s", got, want, flat.String(), grouped.String())
	}
	if dataRowCount(flat.String()) != 2 {
		t.Fatalf("expected 2 rows after -n 2, got %d", dataRowCount(flat.String()))
	}
}

// --by-session has no effect on JSON: grouping is text-only, machine consumers group by session_id.
func TestListBySessionJSONFlat(t *testing.T) {
	f := newFix(t)
	f.write(t, "a", "1")
	f.capture(t, "tool_s1a", "echo s1a", func() { f.write(t, "a", "2") })
	f.session = "sess2"
	f.capture(t, "tool_s2a", "echo s2a", func() { f.write(t, "b", "1") })

	var plain, grouped, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--json"}, &plain, &errb); code != 0 {
		t.Fatalf("json list exit %d: %s", code, errb.String())
	}
	errb.Reset()
	if code := List(f.layout, f.work, []string{"--by-session", "--json"}, &grouped, &errb); code != 0 {
		t.Fatalf("json grouped list exit %d: %s", code, errb.String())
	}
	if plain.String() != grouped.String() {
		t.Fatalf("--by-session --json must equal --json:\n%s\nvs\n%s", plain.String(), grouped.String())
	}
}

// --abs renders group header times as RFC3339.
func TestListBySessionAbs(t *testing.T) {
	f := newFix(t)
	f.write(t, "a", "1")
	f.capture(t, "tool_s1a", "echo s1a", func() { f.write(t, "a", "2") })

	var out, errb bytes.Buffer
	if code := List(f.layout, f.work, []string{"--by-session", "--abs"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "2026-06-10T00:00") {
		t.Fatalf("--abs group header should show RFC3339 time: %q", out.String())
	}
}
