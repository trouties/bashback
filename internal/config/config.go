// Package config resolves bashback's effective settings by overlaying three
// layers: built-in defaults, environment variables, and the per-project
// meta.json — the most specific layer wins. Every parse is fail-open:
// a malformed value falls back to the lower layer rather than erroring, so a bad
// override never breaks a hook.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/paths"
)

// Built-in defaults. They live here, not in the consuming packages,
// so config has no import cycle with snapshot/daemon.
const (
	DefaultStaleTTL      = 15 * time.Minute
	DefaultIdleTimeout   = 30 * time.Minute
	DefaultMaxFileBytes  = int64(100) << 20
	DefaultRetentionDays = 14
	DefaultSoftCapBytes  = int64(2) << 30
	// "major": inject a one-time SessionStart hint plus large/destructive-change
	// warnings, but no per-command noise.
	DefaultContextFeedback = "major"
)

// Source labels where an effective value came from, for `doctor` display.
const (
	SourceDefault = "default"
	SourceEnv     = "env"
	SourceProject = "project"
)

// Env var names.
const (
	EnvStaleTTL     = "BASHBACK_STALE_TTL"
	EnvIdleTimeout  = "BASHBACK_IDLE_TIMEOUT"
	EnvMaxFileBytes = "BASHBACK_MAX_FILE_BYTES"
)

// Config is the resolved effective configuration plus the source of each value.
type Config struct {
	StaleTTL        time.Duration
	IdleTimeout     time.Duration
	MaxFileBytes    int64
	RetentionDays   int
	SoftCapBytes    int64
	ContextFeedback string
	ProtectPaths    []string
	ForceInclude    []string
	Sources         map[string]string
}

// Retention is the GC age threshold derived from RetentionDays.
func (c Config) Retention() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}

// Env is an injectable environment snapshot.
type Env map[string]string

// OSEnv reads the bashback env vars from the process environment.
func OSEnv() Env {
	e := Env{}
	for _, k := range []string{EnvStaleTTL, EnvIdleTimeout, EnvMaxFileBytes} {
		if v := os.Getenv(k); v != "" {
			e[k] = v
		}
	}
	return e
}

// Load reads the project meta.json (fail-open to empty on error) and resolves.
func Load(layout paths.Layout, workdir string, env Env) Config {
	meta, err := layout.ReadMeta(workdir)
	if err != nil {
		meta = paths.Meta{}
	}
	return Resolve(meta, env)
}

// Resolve overlays default → env → project. A value present (and parseable) at a
// higher-priority layer overrides the lower one; an unparseable override is
// ignored (fail-open).
func Resolve(meta paths.Meta, env Env) Config {
	c := Config{
		StaleTTL:        DefaultStaleTTL,
		IdleTimeout:     DefaultIdleTimeout,
		MaxFileBytes:    DefaultMaxFileBytes,
		RetentionDays:   DefaultRetentionDays,
		SoftCapBytes:    DefaultSoftCapBytes,
		ContextFeedback: DefaultContextFeedback,
		Sources: map[string]string{
			"stale_ttl":        SourceDefault,
			"idle_timeout":     SourceDefault,
			"max_file_bytes":   SourceDefault,
			"retention_days":   SourceDefault,
			"soft_cap_bytes":   SourceDefault,
			"context_feedback": SourceDefault,
			"protect_paths":    SourceDefault,
			"force_include":    SourceDefault,
		},
	}

	// Env layer (the three documented vars).
	if d, ok := parseDuration(env[EnvStaleTTL]); ok {
		c.StaleTTL = d
		c.Sources["stale_ttl"] = SourceEnv
	}
	if d, ok := parseDuration(env[EnvIdleTimeout]); ok {
		c.IdleTimeout = d
		c.Sources["idle_timeout"] = SourceEnv
	}
	if b, ok := ParseBytes(env[EnvMaxFileBytes]); ok {
		c.MaxFileBytes = b
		c.Sources["max_file_bytes"] = SourceEnv
	}

	// Project layer (meta.json additive keys).
	if meta.MaxFileBytes > 0 {
		c.MaxFileBytes = meta.MaxFileBytes
		c.Sources["max_file_bytes"] = SourceProject
	}
	if meta.RetentionDays > 0 {
		c.RetentionDays = meta.RetentionDays
		c.Sources["retention_days"] = SourceProject
	}
	if meta.SoftCapBytes > 0 {
		c.SoftCapBytes = meta.SoftCapBytes
		c.Sources["soft_cap_bytes"] = SourceProject
	}
	if meta.ContextFeedback != "" {
		c.ContextFeedback = meta.ContextFeedback
		c.Sources["context_feedback"] = SourceProject
	}
	if len(meta.ProtectPaths) > 0 {
		c.ProtectPaths = meta.ProtectPaths
		c.Sources["protect_paths"] = SourceProject
	}
	if len(meta.ForceInclude) > 0 {
		c.ForceInclude = meta.ForceInclude
		c.Sources["force_include"] = SourceProject
	}
	return c
}

func parseDuration(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// ParseBytes accepts a plain integer or an IEC suffix (KiB/MiB/GiB, also K/M/G).
func ParseBytes(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "GiB"), strings.HasSuffix(v, "G"):
		mult = 1 << 30
		v = strings.TrimSuffix(strings.TrimSuffix(v, "GiB"), "G")
	case strings.HasSuffix(v, "MiB"), strings.HasSuffix(v, "M"):
		mult = 1 << 20
		v = strings.TrimSuffix(strings.TrimSuffix(v, "MiB"), "M")
	case strings.HasSuffix(v, "KiB"), strings.HasSuffix(v, "K"):
		mult = 1 << 10
		v = strings.TrimSuffix(strings.TrimSuffix(v, "KiB"), "K")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}
