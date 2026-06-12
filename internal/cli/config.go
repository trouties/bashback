package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/paths"
)

// configKeys are the writable per-project meta.json keys. Unknown
// keys are rejected so a typo doesn't silently no-op.
var configKeys = map[string]bool{
	"force_include":    true,
	"max_file_bytes":   true,
	"retention_days":   true,
	"soft_cap_bytes":   true,
	"context_feedback": true,
	"protect_paths":    true,
}

// Config reads and writes per-project settings in meta.json.
func Config(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	jsonOut, args := popJSONFlag(args)
	if len(args) == 0 {
		return errf(stderr, "usage: bashback config list|get <key>|set <key> <value>|unset <key>")
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprintln(stderr, "usage: bashback config list|get <key>|set <key> <value>|unset <key>")
		return 2
	case "list":
		return configList(layout, workdir, jsonOut, stdout, stderr)
	case "get":
		if len(args) < 2 {
			return errf(stderr, "usage: bashback config get <key>")
		}
		return configGet(layout, workdir, args[1], jsonOut, stdout, stderr)
	case "set":
		if len(args) < 3 {
			return errf(stderr, "usage: bashback config set <key> <value>")
		}
		return configSet(layout, workdir, args[1], args[2:], stdout, stderr)
	case "unset":
		if len(args) < 2 {
			return errf(stderr, "usage: bashback config unset <key>")
		}
		return configUnset(layout, workdir, args[1], stdout, stderr)
	default:
		return errf(stderr, "unknown config subcommand %q (use list|get|set|unset)", args[0])
	}
}

func readMetaOrEmpty(layout paths.Layout, workdir string) (paths.Meta, error) {
	m, err := layout.ReadMeta(workdir)
	if err != nil {
		// Absent meta is fine (defaults); a parse/version error is real.
		if os.IsNotExist(err) {
			return paths.Meta{OriginalPath: workdir}, nil
		}
		return paths.Meta{}, err
	}
	return m, nil
}

func configList(layout paths.Layout, workdir string, jsonOut bool, stdout, stderr io.Writer) int {
	cfg := config.Load(layout, workdir, config.OSEnv())
	vals := effectiveValues(cfg)
	if jsonOut {
		return emitJSON(stdout, stderr, struct {
			V       int               `json:"v"`
			Values  map[string]string `json:"values"`
			Sources map[string]string `json:"sources"`
		}{outputVersion, vals, cfg.Sources})
	}
	keys := sortedKeys(vals)
	for _, k := range keys {
		fmt.Fprintf(stdout, "%-18s %-20s (%s)\n", k, vals[k], cfg.Sources[k])
	}
	return 0
}

func configGet(layout paths.Layout, workdir, key string, jsonOut bool, stdout, stderr io.Writer) int {
	cfg := config.Load(layout, workdir, config.OSEnv())
	vals := effectiveValues(cfg)
	v, ok := vals[key]
	if !ok {
		return errf(stderr, "unknown config key %q", key)
	}
	if jsonOut {
		return emitJSON(stdout, stderr, struct {
			V      int    `json:"v"`
			Key    string `json:"key"`
			Value  string `json:"value"`
			Source string `json:"source"`
		}{outputVersion, key, v, cfg.Sources[key]})
	}
	fmt.Fprintf(stdout, "%s = %s (%s)\n", key, v, cfg.Sources[key])
	return 0
}

func configSet(layout paths.Layout, workdir, key string, values []string, stdout, stderr io.Writer) int {
	if !configKeys[key] {
		return errf(stderr, "unknown config key %q (writable: %s)", key, strings.Join(sortedSet(configKeys), ", "))
	}
	m, err := readMetaOrEmpty(layout, workdir)
	if err != nil {
		return errf(stderr, "read meta: %v", err)
	}
	if err := applyMetaSet(&m, key, values); err != nil {
		return errf(stderr, "%v", err)
	}
	if err := layout.EnsureRepoDirs(workdir); err != nil {
		return errf(stderr, "ensure dirs: %v", err)
	}
	if err := layout.WriteMeta(workdir, m); err != nil {
		return errf(stderr, "write meta: %v", err)
	}
	if key == "force_include" {
		fmt.Fprintln(stderr, "warning: force_include copies the listed files (which may be secrets) verbatim into the shadow repo (chmod 0700); review before committing")
	}
	fmt.Fprintf(stdout, "set %s = %s\n", key, strings.Join(values, " "))
	return 0
}

func configUnset(layout paths.Layout, workdir, key string, stdout, stderr io.Writer) int {
	if !configKeys[key] {
		return errf(stderr, "unknown config key %q", key)
	}
	m, err := readMetaOrEmpty(layout, workdir)
	if err != nil {
		return errf(stderr, "read meta: %v", err)
	}
	clearMetaKey(&m, key)
	if err := layout.EnsureRepoDirs(workdir); err != nil {
		return errf(stderr, "ensure dirs: %v", err)
	}
	if err := layout.WriteMeta(workdir, m); err != nil {
		return errf(stderr, "write meta: %v", err)
	}
	fmt.Fprintf(stdout, "unset %s\n", key)
	return 0
}

func applyMetaSet(m *paths.Meta, key string, values []string) error {
	switch key {
	case "force_include":
		m.ForceInclude = values
	case "protect_paths":
		m.ProtectPaths = values
	case "context_feedback":
		v := values[0]
		if v != "off" && v != "major" && v != "all" {
			return fmt.Errorf("context_feedback must be off|major|all, got %q", v)
		}
		m.ContextFeedback = v
	case "max_file_bytes":
		b, ok := config.ParseBytes(values[0])
		if !ok {
			return fmt.Errorf("max_file_bytes must be a byte count (e.g. 100MiB), got %q", values[0])
		}
		m.MaxFileBytes = b
	case "soft_cap_bytes":
		b, ok := config.ParseBytes(values[0])
		if !ok {
			return fmt.Errorf("soft_cap_bytes must be a byte count, got %q", values[0])
		}
		m.SoftCapBytes = b
	case "retention_days":
		n, err := strconv.Atoi(values[0])
		if err != nil || n <= 0 {
			return fmt.Errorf("retention_days must be a positive integer, got %q", values[0])
		}
		m.RetentionDays = n
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func clearMetaKey(m *paths.Meta, key string) {
	switch key {
	case "force_include":
		m.ForceInclude = nil
	case "protect_paths":
		m.ProtectPaths = nil
	case "context_feedback":
		m.ContextFeedback = ""
	case "max_file_bytes":
		m.MaxFileBytes = 0
	case "soft_cap_bytes":
		m.SoftCapBytes = 0
	case "retention_days":
		m.RetentionDays = 0
	}
}

// effectiveValues renders the resolved config as displayable strings.
func effectiveValues(c config.Config) map[string]string {
	return map[string]string{
		"stale_ttl":        c.StaleTTL.String(),
		"idle_timeout":     c.IdleTimeout.String(),
		"max_file_bytes":   strconv.FormatInt(c.MaxFileBytes, 10),
		"retention_days":   strconv.Itoa(c.RetentionDays),
		"soft_cap_bytes":   strconv.FormatInt(c.SoftCapBytes, 10),
		"context_feedback": c.ContextFeedback,
		"protect_paths":    strings.Join(c.ProtectPaths, ","),
		"force_include":    strings.Join(c.ForceInclude, ","),
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
