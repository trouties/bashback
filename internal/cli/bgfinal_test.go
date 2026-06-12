package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// A bgfinal entry spans the whole background period ([orig ts, completion]); a
// command from another session inside that window marks it overlapped at read
// time, so restoring it requires --force.
func TestBgFinalOverlapGate(t *testing.T) {
	f := newFix(t)
	now := time.Now()
	origTS := now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)

	// Original backgrounded command: real pre/post + bg_task_id.
	repo, err := f.eng.EnsureRepo(ctx(), f.work, f.session)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "out.log", "")
	pre, err := f.eng.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := f.eng.Post(ctx(), repo, pre, nil)
	if err != nil {
		t.Fatal(err)
	}
	jp := f.layout.JournalPath(f.work)
	if err := journal.Append(jp, journal.Entry{
		ToolUseID: "tbg", SessionID: f.session, TS: origTS,
		Command: journal.RedactCommand("sleep 600 &"),
		PreSHA:  pre.PreSHA, PostSHA: post.PostSHA, Status: post.Status,
		BgTaskID: "taskX", Note: "background",
	}); err != nil {
		t.Fatal(err)
	}

	// Background command writes after backgrounding, then completion is captured.
	f.write(t, "out.log", "finished\n")
	res, err := f.eng.BgFinal(ctx(), f.work, f.session, "taskX", nil)
	if err != nil || !res.Created {
		t.Fatalf("bgfinal not created: %+v err=%v", res, err)
	}

	// A concurrent command from another session inside the background window.
	if err := journal.Append(jp, journal.Entry{
		ToolUseID: "other", SessionID: "sess2",
		TS: now.Add(-1 * time.Minute).UTC().Format(time.RFC3339), DurationMS: 1000,
		PreSHA: "p", PostSHA: "q", Status: journal.StatusProtected,
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"bgfinal_tbg"}, &out, &errb); code == 0 {
		t.Fatal("overlapped bgfinal restore should refuse without --force")
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Fatalf("want --force hint, got %q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Restore(f.layout, f.work, []string{"--force", "bgfinal_tbg"}, &out, &errb); code != 0 {
		t.Fatalf("forced bgfinal restore failed: %s", errb.String())
	}
}
