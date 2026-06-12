package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string // substring expected on stderr
	}{
		{name: "no args prints usage", args: nil, wantCode: 2, wantErr: "usage: bashback"},
		{name: "unknown subcommand", args: []string{"frobnicate"}, wantCode: 2, wantErr: "unknown subcommand"},
		{name: "hook always exits 0 (fail-open) even on empty stdin", args: []string{"hook", "pre"}, wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tt.args, strings.NewReader(""), &out, &errb)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr=%q)", code, tt.wantCode, errb.String())
			}
			if tt.wantErr != "" && !strings.Contains(errb.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want substring %q", errb.String(), tt.wantErr)
			}
		})
	}
}

func TestListRoutesToHandler(t *testing.T) {
	// A known command is routed to its handler (not "not implemented"). With an
	// empty home it reports no snapshots and exits 0.
	t.Setenv("BASHBACK_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"list"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "not implemented") {
		t.Fatal("list should be implemented")
	}
}

func TestUnknownSubcommandIsNotZero(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"definitely-not-a-command"}, strings.NewReader(""), &out, &errb); code == 0 {
		t.Fatalf("unknown subcommand must exit non-zero, got 0")
	}
}

func TestVersionReportsBuildInfo(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var out, errb bytes.Buffer
		code := run([]string{arg}, strings.NewReader(""), &out, &errb)
		if code != 0 {
			t.Fatalf("%s exit %d (stderr=%q)", arg, code, errb.String())
		}
		if !strings.Contains(out.String(), "bashback") {
			t.Errorf("%s output = %q, want it to mention bashback", arg, out.String())
		}
	}
}

func TestTopLevelHelpExitsZeroToStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var out, errb bytes.Buffer
		code := run([]string{arg}, strings.NewReader(""), &out, &errb)
		if code != 0 {
			t.Fatalf("%s exit %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "commands:") {
			t.Errorf("%s should print the command list to stdout, got %q", arg, out.String())
		}
	}
}

func TestSubcommandHelpPrintsUsage(t *testing.T) {
	// -h on a subcommand prints that command's usage to stdout (exit 0) rather
	// than running it.
	var out, errb bytes.Buffer
	code := run([]string{"restore", "-h"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Errorf("restore -h exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage: bashback restore") {
		t.Errorf("restore -h should print its usage to stdout, got stdout=%q", out.String())
	}
}

// Every user-facing subcommand must answer -h/--help with a usage line rather
// than mistaking it for a positional argument. A newly-added subcommand that
// forgets -h handling fails this matrix.
func TestAllSubcommandsHelpMatrix(t *testing.T) {
	t.Setenv("BASHBACK_HOME", t.TempDir())
	cmds := []string{
		"list", "diff", "stats", "show", "log", "export", "restore", "undo",
		"rewind", "snap", "gc", "doctor", "config", "install", "version",
	}
	for _, cmd := range cmds {
		for _, h := range []string{"-h", "--help"} {
			var out, errb bytes.Buffer
			run([]string{cmd, h}, strings.NewReader(""), &out, &errb)
			combined := out.String() + errb.String()
			if !strings.Contains(combined, "usage") && !strings.Contains(combined, "Usage") {
				t.Errorf("%s %s should print a usage line, got %q", cmd, h, combined)
			}
		}
	}
}

func TestUsageMentionsMultiPlatformInstall(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	for _, want := range []string{"--codex", "--cursor", "--no-skill", "agent hook entrypoints"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage missing %q", want)
		}
	}
}
