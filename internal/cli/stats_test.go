package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

func TestStatsCounts(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_p1", "x", func() { f.write(t, "f.txt", "v1") }) // protected
	f.capture(t, "tool_p2", "y", func() { f.write(t, "g.txt", "v1") }) // protected
	f.capture(t, "tool_noop", "z", nil)                                // skipped_no_change
	f.preCapture(t, "tool_pre", "interrupted", func() { f.write(t, "f.txt", "half") })

	var out, errb bytes.Buffer
	if code := Stats(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if int(m["total"].(float64)) != 4 {
		t.Fatalf("total = %v, want 4", m["total"])
	}
	if int(m["pre_only"].(float64)) != 1 {
		t.Fatalf("pre_only = %v, want 1", m["pre_only"])
	}
	byStatus := m["by_status"].(map[string]any)
	if int(byStatus["protected"].(float64)) != 2 {
		t.Fatalf("protected = %v, want 2", byStatus["protected"])
	}
	if int(byStatus["skipped_no_change"].(float64)) != 1 {
		t.Fatalf("skipped = %v, want 1", byStatus["skipped_no_change"])
	}
	// coverage = (protected 2 + skipped 1 + restored 0) / 4 = 0.75
	if cov := m["coverage_rate"].(float64); cov < 0.74 || cov > 0.76 {
		t.Fatalf("coverage = %v, want 0.75", cov)
	}
}

func TestStatsTopChurn(t *testing.T) {
	f := newFix(t)
	f.write(t, "hot.txt", "v0")
	f.capture(t, "tool_1", "x", func() { f.write(t, "hot.txt", "v1") })
	f.capture(t, "tool_2", "x", func() { f.write(t, "hot.txt", "v2") })
	f.capture(t, "tool_3", "x", func() { f.write(t, "cold.txt", "c") })

	var out, errb bytes.Buffer
	if code := Stats(f.layout, f.work, []string{"--json"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	top := m["top_churn_files"].([]any)
	if len(top) == 0 {
		t.Fatal("expected churn files")
	}
	first := top[0].(map[string]any)
	if first["path"] != "hot.txt" || int(first["changes"].(float64)) != 2 {
		t.Fatalf("top churn = %v, want hot.txt x2", first)
	}
}

func TestStatsText(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_x", "x", func() { f.write(t, "f.txt", "v1") })
	var out, errb bytes.Buffer
	if code := Stats(f.layout, f.work, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	for _, want := range []string{"entries:", "coverage:", "disk usage:", "journal size:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stats text missing %q", want)
		}
	}
}

func TestDiffStat(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "a\nb\n")
	key := f.capture(t, "tool_ds", "x", func() { f.write(t, "f.txt", "a\nb\nc\nd\n") })

	// Text form.
	var out, errb bytes.Buffer
	if code := Diff(f.layout, f.work, []string{"--stat", key}, &out, &errb); code != 0 {
		t.Fatalf("diff --stat exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "f.txt") || !strings.Contains(s, "+2") {
		t.Fatalf("diff --stat should report +2 for f.txt: %q", s)
	}

	// JSON form: numbers agree.
	out.Reset()
	errb.Reset()
	if code := Diff(f.layout, f.work, []string{"--stat", "--json", key}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	if int(m["added"].(float64)) != 2 || int(m["deleted"].(float64)) != 0 {
		t.Fatalf("stat totals = +%v -%v, want +2 -0", m["added"], m["deleted"])
	}
	files := m["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	fc := files[0].(map[string]any)
	if fc["status"] != "M" || int(fc["added"].(float64)) != 2 {
		t.Fatalf("file stat = %v", fc)
	}
}

// diff --stat numbers must agree with the patch form for the same entry.
func TestDiffStatMatchesPatch(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "x\n")
	key := f.capture(t, "tool_m", "x", func() { f.write(t, "f.txt", "x\ny\nz\n") })

	var patchOut, errb bytes.Buffer
	Diff(f.layout, f.work, []string{key}, &patchOut, &errb)
	addedInPatch := strings.Count(patchOut.String(), "\n+") // +lines (incl +++ header)

	var statOut bytes.Buffer
	Diff(f.layout, f.work, []string{"--stat", "--json", key}, &statOut, &errb)
	m := decodeJSON(t, statOut.Bytes())
	added := int(m["added"].(float64))
	// patch has the +++ header line plus the added content lines.
	if added != addedInPatch-1 {
		t.Fatalf("stat added %d should match patch added lines %d", added, addedInPatch-1)
	}
	_ = journal.StatusProtected
}

func TestStatsRejectsUnknownFlag(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := Stats(f.layout, f.work, []string{"--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("stats --bogus exit = %d, want 2", code)
	}
}

func TestExplicitHelpGoesToStdoutExitZero(t *testing.T) {
	f := newFix(t)
	type cmd func(paths.Layout, string, []string, io.Writer, io.Writer) int
	cmds := map[string]cmd{"list": List, "diff": Diff, "stats": Stats, "install": Install, "restore": Restore}
	for name, fn := range cmds {
		var out, errb bytes.Buffer
		code := fn(f.layout, f.work, []string{"-h"}, &out, &errb)
		if code != 0 {
			t.Errorf("%s -h exit = %d, want 0", name, code)
		}
		if errb.Len() != 0 {
			t.Errorf("%s -h wrote to stderr: %q", name, errb.String())
		}
		if !strings.Contains(strings.ToLower(out.String()), "usage") {
			t.Errorf("%s -h stdout missing usage: %q", name, out.String())
		}
	}
}

func TestInstallHelpListsPlatformFlags(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := Install(f.layout, f.work, []string{"-h"}, &out, &errb); code != 0 {
		t.Fatalf("install -h exit = %d, want 0", code)
	}
	for _, want := range []string{"--codex", "--cursor"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install -h stdout missing %q: %q", want, out.String())
		}
	}
}
