package config

import (
	"testing"

	"github.com/trouties/bashback/internal/paths"
)

func TestResolveDefaults(t *testing.T) {
	c := Resolve(paths.Meta{}, Env{})
	if c.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("max_file_bytes = %d, want default", c.MaxFileBytes)
	}
	if c.StaleTTL != DefaultStaleTTL || c.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("TTL defaults wrong: %v %v", c.StaleTTL, c.IdleTimeout)
	}
	if c.RetentionDays != DefaultRetentionDays || c.ContextFeedback != DefaultContextFeedback {
		t.Fatalf("defaults wrong: %d %s", c.RetentionDays, c.ContextFeedback)
	}
	if c.Sources["max_file_bytes"] != SourceDefault {
		t.Fatalf("source = %s, want default", c.Sources["max_file_bytes"])
	}
}

func TestEnvOverridesDefault(t *testing.T) {
	env := Env{
		EnvStaleTTL:     "5m",
		EnvIdleTimeout:  "10m",
		EnvMaxFileBytes: "50MiB",
	}
	c := Resolve(paths.Meta{}, env)
	if c.StaleTTL.Minutes() != 5 || c.IdleTimeout.Minutes() != 10 {
		t.Fatalf("env TTLs not applied: %v %v", c.StaleTTL, c.IdleTimeout)
	}
	if c.MaxFileBytes != 50<<20 {
		t.Fatalf("env max_file_bytes = %d, want 50MiB", c.MaxFileBytes)
	}
	if c.Sources["max_file_bytes"] != SourceEnv {
		t.Fatalf("source = %s, want env", c.Sources["max_file_bytes"])
	}
}

// Project meta wins over both env and default.
func TestProjectOverridesEnvAndDefault(t *testing.T) {
	env := Env{EnvMaxFileBytes: "50MiB"}
	meta := paths.Meta{MaxFileBytes: 7 << 20, RetentionDays: 30}
	c := Resolve(meta, env)
	if c.MaxFileBytes != 7<<20 {
		t.Fatalf("project should win: max_file_bytes = %d, want 7MiB", c.MaxFileBytes)
	}
	if c.Sources["max_file_bytes"] != SourceProject {
		t.Fatalf("source = %s, want project", c.Sources["max_file_bytes"])
	}
	if c.RetentionDays != 30 || c.Sources["retention_days"] != SourceProject {
		t.Fatalf("retention from project not applied: %d (%s)", c.RetentionDays, c.Sources["retention_days"])
	}
}

// A malformed env value is ignored (fail-open), keeping the default.
func TestFailOpenOnBadValue(t *testing.T) {
	c := Resolve(paths.Meta{}, Env{EnvMaxFileBytes: "not-a-number"})
	if c.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("bad env value should fall back to default, got %d", c.MaxFileBytes)
	}
	if c.Sources["max_file_bytes"] != SourceDefault {
		t.Fatalf("source = %s, want default", c.Sources["max_file_bytes"])
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"100":    100,
		"100MiB": 100 << 20,
		"2GiB":   2 << 30,
		"4KiB":   4 << 10,
		"512M":   512 << 20,
	}
	for in, want := range cases {
		got, ok := ParseBytes(in)
		if !ok || got != want {
			t.Errorf("ParseBytes(%q) = %d ok=%v, want %d", in, got, ok, want)
		}
	}
	if _, ok := ParseBytes("abc"); ok {
		t.Error("ParseBytes(abc) should fail")
	}
}
