package hook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decodeEnvelope(t *testing.T, b []byte) (event, ctx string) {
	t.Helper()
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\n%s", err, b)
	}
	return out.HookSpecificOutput.HookEventName, out.HookSpecificOutput.AdditionalContext
}

// off injects nothing, even for a large change or session start.
func TestInjectOffSilent(t *testing.T) {
	for _, op := range []string{"session-start", "post"} {
		var out bytes.Buffer
		emitContext(&out, op, "off", 50, 10, "toolu_x", "", "", 0)
		if out.Len() != 0 {
			t.Fatalf("off tier must emit nothing for %s, got %q", op, out.String())
		}
	}
}

// all injects on every post that changed files, and on session start.
func TestInjectAll(t *testing.T) {
	var out bytes.Buffer
	emitContext(&out, "post", "all", 2, 0, "toolu_a", "", "", 0)
	ev, ctx := decodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
	if ev != "PostToolUse" || !strings.Contains(ctx, "2 files") || !strings.Contains(ctx, "toolu_a") {
		t.Fatalf("all/post envelope wrong: ev=%s ctx=%q", ev, ctx)
	}

	out.Reset()
	emitContext(&out, "session-start", "all", 0, 0, "", "", "", 0)
	ev, ctx = decodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
	if ev != "SessionStart" || !strings.Contains(ctx, "bashback") {
		t.Fatalf("all/session-start envelope wrong: ev=%s ctx=%q", ev, ctx)
	}

	// A no-op post (zero files) injects nothing even under `all`.
	out.Reset()
	emitContext(&out, "post", "all", 0, 0, "toolu_noop", "", "", 0)
	if out.Len() != 0 {
		t.Fatalf("all tier must skip zero-change posts, got %q", out.String())
	}
}

// major injects on session start and only on large posts: files >= threshold OR
// any deletion.
func TestInjectMajorThreshold(t *testing.T) {
	cases := []struct {
		files, deletes int
		want           bool
	}{
		{1, 0, false},
		{MajorFilesThreshold - 1, 0, false},
		{MajorFilesThreshold, 0, true},
		{2, 1, true}, // small but has a deletion
	}
	for _, c := range cases {
		var out bytes.Buffer
		emitContext(&out, "post", "major", c.files, c.deletes, "toolu_m", "", "", 0)
		got := out.Len() > 0
		if got != c.want {
			t.Fatalf("major files=%d deletes=%d emit=%v want=%v (%q)", c.files, c.deletes, got, c.want, out.String())
		}
		if got {
			_, ctx := decodeEnvelope(t, bytes.TrimSpace(out.Bytes()))
			if c.deletes > 0 && !strings.Contains(ctx, "deleted") {
				t.Fatalf("deletion count should surface: %q", ctx)
			}
		}
	}

	var out bytes.Buffer
	emitContext(&out, "session-start", "major", 0, 0, "", "", "", 0)
	if out.Len() == 0 {
		t.Fatal("major tier should still inject the one-time SessionStart hint")
	}
}

// changeLine is diff-first: it points the agent at `diff` to review before
// `restore` to undo, and never repeats the key as a parenthetical "undo key".
func TestChangeLineDiffFirst(t *testing.T) {
	got := changeLine(12, 3, "toolu_x")
	for _, want := range []string{"review: bashback diff toolu_x", "undo: bashback restore toolu_x", "3 deleted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("changeLine missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "undo key") {
		t.Fatalf("changeLine should not repeat the key as an undo key: %q", got)
	}
	// No-deletion form drops the parenthetical but keeps the diff-first guidance.
	nd := changeLine(2, 0, "toolu_y")
	if strings.Contains(nd, "deleted") || strings.Contains(nd, "undo key") {
		t.Fatalf("no-deletion changeLine wrong: %q", nd)
	}
	if !strings.Contains(nd, "review: bashback diff toolu_y") || !strings.Contains(nd, "undo: bashback restore toolu_y") {
		t.Fatalf("no-deletion changeLine missing diff-first guidance: %q", nd)
	}
}

// bgChangeLine carries the same diff-first guidance and still announces the
// background completion.
func TestBgChangeLineDiffFirst(t *testing.T) {
	got := bgChangeLine(12, 3, "toolu_x")
	for _, want := range []string{"background command finished", "review: bashback diff toolu_x", "undo: bashback restore toolu_x", "3 deleted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("bgChangeLine missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "undo key") {
		t.Fatalf("bgChangeLine should not repeat the key as an undo key: %q", got)
	}
}

// Injected lines stay terminal-friendly: under 200 chars even with a long key.
func TestInjectLineLength(t *testing.T) {
	longKey := strings.Repeat("k", 40)
	lines := map[string]string{
		"changeLine":   changeLine(10, 3, longKey),
		"bgChangeLine": bgChangeLine(10, 3, longKey),
		"sessionHint":  sessionHintText(strings.Repeat("s", 36)),
	}
	for name, line := range lines {
		if len(line) > 200 {
			t.Fatalf("%s is %d chars (>200): %q", name, len(line), line)
		}
	}
}

// A resume/compact SessionStart for a session that already has snapshots gets a
// count-aware reminder pointing at the prior history.
func TestSessionStartResumeInjectsCount(t *testing.T) {
	for _, src := range []string{"resume", "compact"} {
		_, text, ok := contextMessage("session-start", "all", 0, 0, "", src, "", 5)
		if !ok {
			t.Fatalf("%s session-start should inject", src)
		}
		if !strings.Contains(text, "5 snapshot entries") || !strings.Contains(text, "bashback list") {
			t.Fatalf("%s hint missing count/list: %q", src, text)
		}
	}
}

// startup/clear/empty/unknown sources fall back to the generic one-time hint
// regardless of entry count.
func TestSessionStartStartupClearUnknown(t *testing.T) {
	for _, src := range []string{"startup", "clear", "", "whatever"} {
		_, text, ok := contextMessage("session-start", "all", 0, 0, "", src, "", 5)
		if !ok || text != sessionHintText("") {
			t.Fatalf("source %q should yield sessionHint, got ok=%v %q", src, ok, text)
		}
	}
}

// resume with no prior entries still gets the generic hint, not a "0 entries"
// reminder.
func TestSessionStartResumeZeroEntries(t *testing.T) {
	_, text, ok := contextMessage("session-start", "all", 0, 0, "", "resume", "", 0)
	if !ok || text != sessionHintText("") {
		t.Fatalf("resume+0 should yield sessionHint, got ok=%v %q", ok, text)
	}
}

// The SessionStart hint carries a 12-char session id prefix so the agent can
// scope --session without guessing which active session is its own.
func TestSessionStartCarriesSessionID(t *testing.T) {
	const sid = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"
	_, text, ok := contextMessage("session-start", "major", 0, 0, "", "startup", sid, 0)
	if !ok || !strings.Contains(text, "(session 0a1b2c3d-4e5)") {
		t.Fatalf("hint should carry a 12-char session prefix: %q", text)
	}
	_, text, _ = contextMessage("session-start", "all", 0, 0, "", "resume", sid, 7)
	if !strings.Contains(text, "(session 0a1b2c3d-4e5)") || !strings.Contains(text, "7 snapshot entries") {
		t.Fatalf("resume hint should carry id and count: %q", text)
	}
}

// off stays silent across every source/entry combination.
func TestSessionStartOffSilent(t *testing.T) {
	for _, src := range []string{"resume", "compact", "startup"} {
		if _, _, ok := contextMessage("session-start", "off", 0, 0, "", src, "", 9); ok {
			t.Fatalf("off must inject nothing for source %q", src)
		}
	}
}

// An unknown tier is treated as off (fail-safe default).
func TestInjectUnknownTierSilent(t *testing.T) {
	var out bytes.Buffer
	emitContext(&out, "post", "weird", 99, 9, "toolu_z", "", "", 0)
	if out.Len() != 0 {
		t.Fatalf("unknown tier must inject nothing, got %q", out.String())
	}
}

// cursor injects its session-start orientation as the {"additional_context": ...}
// envelope, but stays silent on post/bg (the Claude-style envelope is not emitted
// on cursor).
func TestCursorEnvelopes(t *testing.T) {
	var out bytes.Buffer
	emitContextFor("cursor", &out, "session-start", "all", 0, 0, "", "startup", "cv-1", 0)
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || env["additional_context"] == nil {
		t.Fatalf("cursor session-start must emit additional_context: %q", out.String())
	}
	out.Reset()
	emitContextFor("cursor", &out, "post", "all", 3, 1, "k", "", "", 0)
	if out.Len() != 0 {
		t.Fatalf("cursor post must stay silent: %q", out.String())
	}
	out.Reset()
	emitContextFor("cursor", &out, "bg", "all", 3, 1, "k", "", "", 0)
	if out.Len() != 0 {
		t.Fatalf("cursor bg must stay silent: %q", out.String())
	}
}

// cursor before-hooks get an explicit allow envelope (belt-and-braces; empty
// stdout would also allow).
func TestCursorPreAllow(t *testing.T) {
	var out bytes.Buffer
	emitCursorAllow(&out)
	if strings.TrimSpace(out.String()) != `{"permission":"allow"}` {
		t.Fatalf("got %q", out.String())
	}
}

func TestChangeLineSingularFile(t *testing.T) {
	got := changeLine(1, 1, "k")
	if !strings.Contains(got, "changed 1 file (1 deleted)") || strings.Contains(got, "1 files") {
		t.Fatalf("changeLine(1,1) = %q, want singular 'file'", got)
	}
	if many := changeLine(3, 0, "k"); !strings.Contains(many, "3 files") {
		t.Fatalf("changeLine(3,0) = %q, want plural 'files'", many)
	}
}
