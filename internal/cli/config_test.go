package cli

import (
	"bytes"
	"strings"
	"testing"
)

// -h/--help on config prints usage to stderr and never falls into the
// `unknown config subcommand "-h"` path.
func TestConfigHelpFlag(t *testing.T) {
	f := newFix(t)
	for _, h := range []string{"-h", "--help"} {
		var out, errb bytes.Buffer
		code := Config(f.layout, f.work, []string{h}, &out, &errb)
		if code != 2 {
			t.Errorf("config %s exit = %d, want 2", h, code)
		}
		if !strings.Contains(errb.String(), "usage") {
			t.Errorf("config %s should print usage to stderr, got %q", h, errb.String())
		}
		if strings.Contains(out.String()+errb.String(), "unknown config subcommand") {
			t.Errorf("config %s fell into the subcommand path: %q", h, out.String()+errb.String())
		}
	}
}

func TestConfigSetGetUnset(t *testing.T) {
	f := newFix(t)

	var out, errb bytes.Buffer
	if code := Config(f.layout, f.work, []string{"set", "retention_days", "30"}, &out, &errb); code != 0 {
		t.Fatalf("set exit %d: %s", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Config(f.layout, f.work, []string{"get", "retention_days"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "30") || !strings.Contains(out.String(), "project") {
		t.Fatalf("get after set: %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Config(f.layout, f.work, []string{"unset", "retention_days"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	// After unset, the value reverts to the default source.
	out.Reset()
	errb.Reset()
	Config(f.layout, f.work, []string{"get", "retention_days"}, &out, &errb)
	if !strings.Contains(out.String(), "default") {
		t.Fatalf("after unset should be default: %q", out.String())
	}
}

func TestConfigUnknownKeyRejected(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := Config(f.layout, f.work, []string{"set", "retenton_days", "30"}, &out, &errb); code == 0 {
		t.Fatal("typo'd key should be rejected")
	}
	if !strings.Contains(errb.String(), "unknown config key") {
		t.Fatalf("want unknown-key error, got %q", errb.String())
	}
}

func TestConfigForceIncludeWarns(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := Config(f.layout, f.work, []string{"set", "force_include", ".env"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(errb.String(), "secret") {
		t.Fatalf("force_include should warn about secrets: %q", errb.String())
	}
}

func TestConfigContextFeedbackValidation(t *testing.T) {
	f := newFix(t)
	var out, errb bytes.Buffer
	if code := Config(f.layout, f.work, []string{"set", "context_feedback", "bogus"}, &out, &errb); code == 0 {
		t.Fatal("invalid context_feedback should be rejected")
	}
	out.Reset()
	errb.Reset()
	if code := Config(f.layout, f.work, []string{"set", "context_feedback", "major"}, &out, &errb); code != 0 {
		t.Fatalf("valid value should be accepted: %s", errb.String())
	}
}

func TestConfigListJSON(t *testing.T) {
	f := newFix(t)
	Config(f.layout, f.work, []string{"set", "max_file_bytes", "50MiB"}, &bytes.Buffer{}, &bytes.Buffer{})

	var out, errb bytes.Buffer
	if code := Config(f.layout, f.work, []string{"--json", "list"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	m := decodeJSON(t, out.Bytes())
	vals := m["values"].(map[string]any)
	if vals["max_file_bytes"] != "52428800" {
		t.Fatalf("max_file_bytes = %v, want 52428800", vals["max_file_bytes"])
	}
	srcs := m["sources"].(map[string]any)
	if srcs["max_file_bytes"] != "project" {
		t.Fatalf("source = %v, want project", srcs["max_file_bytes"])
	}
}

// A project max_file_bytes set via config actually changes snapshot behavior.
func TestConfigMaxFileBytesTakesEffect(t *testing.T) {
	f := newFix(t)
	Config(f.layout, f.work, []string{"set", "max_file_bytes", "16"}, &bytes.Buffer{}, &bytes.Buffer{})

	f.write(t, "base.txt", "x")
	f.write(t, "big.bin", "this is definitely larger than sixteen bytes")
	// Capture using the production-wired engine (newEngine sets MaxFileBytesFor).
	eng := newEngine(f.layout)
	repo, err := eng.EnsureRepo(ctx(), f.work, f.session)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := eng.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := eng.Post(ctx(), repo, pre, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The oversized exclusion is noted by whichever snapshot first sees big.bin
	// (here the baseline pre).
	if !strings.Contains(pre.Note+post.Note, "oversized") {
		t.Fatalf("config max_file_bytes should exclude big.bin: pre=%q post=%q", pre.Note, post.Note)
	}
}

// doctor surfaces effective config values with their source labels.
func TestDoctorShowsConfigSources(t *testing.T) {
	f := newFix(t)
	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	Config(f.layout, f.work, []string{"set", "retention_days", "21"}, &bytes.Buffer{}, &bytes.Buffer{})

	var out, errb bytes.Buffer
	Doctor(f.layout, f.work, nil, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "retention_days: 21 (project)") {
		t.Fatalf("doctor should show project-sourced retention: %q", s)
	}
	if !strings.Contains(s, "max_file_bytes") || !strings.Contains(s, "(default)") {
		t.Fatalf("doctor should show config with sources: %q", s)
	}
}
