package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// show on a background entry with a captured bgfinal points at it as a next step.
func TestShowBackgroundPointsToBgFinal(t *testing.T) {
	f := newFix(t)
	f.write(t, "out.log", "")
	key := f.capture(t, "tbg", "sleep 600 &", nil)
	jp := f.layout.JournalPath(f.work)
	// Mark the original entry background and add its bgfinal capture.
	if err := journal.Append(jp, journal.Entry{ToolUseID: key, Note: "background"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(jp, journal.Entry{
		ToolUseID: "bgfinal_tbg", SessionID: f.session, TS: "2026-06-10T00:01:00Z",
		PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
		Command: "background completion of: sleep 600 &",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Show(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("show exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "bashback diff bgfinal_tbg") {
		t.Fatalf("show should point at the bgfinal entry: %q", out.String())
	}
}

// -h/--help prints usage to stderr, never falling into the key-argument path
// that would report `no entry with key "-h"`.
func TestShowHelpFlag(t *testing.T) {
	f := newFix(t)
	for _, h := range []string{"-h", "--help"} {
		var out, errb bytes.Buffer
		code := Show(f.layout, f.work, []string{h}, &out, &errb)
		if code != 0 {
			t.Errorf("show %s exit = %d, want 0", h, code)
		}
		if !strings.Contains(out.String(), "usage") {
			t.Errorf("show %s should print usage to stdout, got %q", h, out.String())
		}
		if strings.Contains(out.String()+errb.String(), "no entry with key") {
			t.Errorf("show %s fell into the positional path: %q", h, out.String()+errb.String())
		}
	}
}

func TestShowTextAllFields(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "v0")
	key := f.capture(t, "tool_show", "echo hi", func() {
		f.write(t, "a.txt", "v1")
		f.write(t, "b.txt", "new")
	})

	var out, errb bytes.Buffer
	if code := Show(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatalf("show exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"tool_show", "session:", "pre_sha:", "post_sha:", "files", "next steps", "bashback diff", "bashback rewind"} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q:\n%s", want, s)
		}
	}
	// The changed files appear with their status letters.
	if !strings.Contains(s, "a.txt") || !strings.Contains(s, "b.txt") {
		t.Fatalf("files not listed: %q", s)
	}
}

func TestShowJSON(t *testing.T) {
	f := newFix(t)
	f.write(t, "a.txt", "v0")
	key := f.capture(t, "tool_showj", "x", func() { f.write(t, "a.txt", "v1") })

	var out, errb bytes.Buffer
	if code := Show(f.layout, f.work, []string{"--json", key}, &out, &errb); code != 0 {
		t.Fatalf("show --json exit %d: %s", code, errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	for _, k := range []string{"key", "session_id", "ts", "status", "pre_sha", "post_sha", "files", "next_steps", "overlapped", "pre_only", "reclaimed"} {
		if _, ok := m[k]; !ok {
			t.Errorf("show --json missing %q", k)
		}
	}
}

// A pre-only entry's next steps point at restore --force and rewind.
func TestShowPreOnlyNextSteps(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	key := f.preCapture(t, "tool_pre", "interrupted", func() { f.write(t, "f.txt", "half") })

	var out, errb bytes.Buffer
	if code := Show(f.layout, f.work, []string{key}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "--force") || !strings.Contains(s, "rewind") {
		t.Fatalf("pre-only next steps should mention restore --force and rewind: %q", s)
	}
}

func TestShowSurfacesOrigin(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	jp := f.layout.JournalPath(f.work)
	if err := journal.Append(jp, journal.Entry{
		ToolUseID: "tool_origin", SessionID: f.session, TS: f.nextTS(),
		Status: journal.StatusProtected, PreSHA: "a", PostSHA: "b", Origin: "codex",
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Show(f.layout, f.work, []string{"tool_origin"}, &out, &errb); code != 0 {
		t.Fatalf("show exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "origin:") || !strings.Contains(out.String(), "codex") {
		t.Errorf("show text missing origin line: %s", out.String())
	}
	var jo, je bytes.Buffer
	if code := Show(f.layout, f.work, []string{"--json", "tool_origin"}, &jo, &je); code != 0 {
		t.Fatalf("show --json exit %d: %s", code, je.String())
	}
	if !strings.Contains(jo.String(), `"origin": "codex"`) {
		t.Errorf("show --json missing origin: %s", jo.String())
	}
}
