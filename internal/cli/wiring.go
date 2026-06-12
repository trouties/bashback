package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// wiringStatus is one row of doctor's wiring section: whether a given hook is
// actually wired into a platform's settings file, and the binary it points at.
// File is set only for an `unreadable` row. Platform identifies which agent
// platform owns this row ("claude", "codex", "cursor"). File and Drift stay
// out of the JSON schema (text-only signals).
type wiringStatus struct {
	Event    string `json:"hook"`
	Matcher  string `json:"matcher"`
	Status   string `json:"status"` // ok | missing | stale path | unreadable | not wired
	Command  string `json:"command,omitempty"`
	Platform string `json:"platform"`
	File     string `json:"-"`
	Drift    bool   `json:"-"` // the wired binary differs from the running one
}

// expectedWirings is the set of hooks bashback needs. Op is the
// bashback subcommand the command must end in (`… hook <op>`); matching keys on
// that op-suffix, NOT on Matcher, so alias ordering in the bg matcher string
// ("TaskOutput|TaskStop|BashOutput|KillShell") never causes a false negative.
// Matcher here is the display/identity value only.
type expectedWiring struct{ Event, Matcher, Op string }

// wiringEntry is a resolved hook command found in a settings file.
type wiringEntry struct{ Matcher, Command string }

var expectedWirings = []expectedWiring{
	{"PreToolUse", "Bash", "pre"},
	{"PostToolUse", "Bash", "post"},
	{"PostToolUse", "TaskOutput|TaskStop|BashOutput|KillShell", "bg"},
	{"PostToolUseFailure", "Bash", "post"},
	{"SessionStart", "", "session-start"},
	{"SessionEnd", "", "session-end"},
}

// hookMatcher / hookCmd mirror the Claude settings.json hooks block. Shared by
// the wiring check and `bashback install`.
type hookMatcher struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// settingsCandidates returns the existing Claude settings files that form the
// merged view: each ancestor dir's .claude/settings.json and
// settings.local.json from workdir up to the filesystem root, then the
// user-level ~/.claude/settings.json. Only files that exist are returned.
func settingsCandidates(workdir, home string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			out = append(out, p)
		}
	}
	dir := filepath.Clean(workdir)
	for {
		add(filepath.Join(dir, ".claude", "settings.json"))
		add(filepath.Join(dir, ".claude", "settings.local.json"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if home != "" {
		add(filepath.Join(home, ".claude", "settings.json"))
	}
	return out
}

// parseHooksFile decodes the `hooks` block of a settings.json. A file with no
// `hooks` key is valid (empty map); malformed JSON is an error the caller
// surfaces as `unreadable` rather than crashing on.
func parseHooksFile(path string) (map[string][]hookMatcher, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	raw, ok := top["hooks"]
	if !ok {
		return map[string][]hookMatcher{}, nil
	}
	var hooks map[string][]hookMatcher
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return nil, err
	}
	return hooks, nil
}

// checkWiring reports, for each expected hook, whether some candidate settings
// file wires it and whether the wired binary still resolves. An
// unreadable settings file becomes its own row but does not stop the scan of the
// others. selfExe is the running binary's path; a wired command pointing at a
// different binary is flagged as drift (text-only, never a failure).
func checkWiring(workdir, home, selfExe string) []wiringStatus {
	type entry struct{ Matcher, Command string }
	merged := map[string][]entry{}
	var statuses []wiringStatus

	for _, path := range settingsCandidates(workdir, home) {
		hooks, err := parseHooksFile(path)
		if err != nil {
			statuses = append(statuses, wiringStatus{Status: "unreadable", File: path, Platform: "claude"})
			continue
		}
		for event, arr := range hooks {
			for _, m := range arr {
				for _, hk := range m.Hooks {
					merged[event] = append(merged[event], entry{Matcher: m.Matcher, Command: hk.Command})
				}
			}
		}
	}

	for _, exp := range expectedWirings {
		st := wiringStatus{Event: exp.Event, Matcher: exp.Matcher, Status: "missing", Platform: "claude"}
		needle := " hook " + exp.Op
		for _, ent := range merged[exp.Event] {
			if !strings.Contains(ent.Command, needle) {
				continue
			}
			st.Command = ent.Command
			st.Status = "ok"
			tok := firstToken(ent.Command)
			if !resolvesToExecutable(tok) {
				st.Status = "stale path"
			}
			if tok != "" && tok != selfExe {
				st.Drift = true
			}
			break
		}
		statuses = append(statuses, st)
	}
	return statuses
}

// parseCursorHooksFile decodes the `hooks` block of a Cursor hooks.json.
// Cursor uses a flatter shape than Claude/Codex: top-level {version, hooks: {event: [{matcher?, command}]}}.
func parseCursorHooksFile(path string) (map[string][]cursorHookEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	raw, ok := top["hooks"]
	if !ok {
		return map[string][]cursorHookEntry{}, nil
	}
	var hooks map[string][]cursorHookEntry
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return nil, err
	}
	return hooks, nil
}

// classifyWirings matches a merged event map against an expected wiring table,
// returning one wiringStatus per entry and the count that were found (ok or stale).
func classifyWirings(merged map[string][]wiringEntry, wirings []expectedWiring, platform, selfExe string) ([]wiringStatus, int) {
	var rows []wiringStatus
	found := 0
	for _, exp := range wirings {
		st := wiringStatus{Event: exp.Event, Matcher: exp.Matcher, Status: "missing", Platform: platform}
		needle := " hook " + exp.Op
		for _, ent := range merged[exp.Event] {
			if !strings.Contains(ent.Command, needle) {
				continue
			}
			st.Command = ent.Command
			st.Status = "ok"
			tok := firstToken(ent.Command)
			if !resolvesToExecutable(tok) {
				st.Status = "stale path"
			}
			if tok != "" && tok != selfExe {
				st.Drift = true
			}
			found++
			break
		}
		rows = append(rows, st)
	}
	return rows, found
}

// checkCodexWiring reports wiring status for the Codex platform. It checks the
// project-level and user-level .codex/hooks.json files. If no expected wiring
// is found at all and no file is unreadable, it collapses to a single
// "not wired" row (informational — Codex is optional).
func checkCodexWiring(workdir, home, selfExe string) []wiringStatus {
	merged := map[string][]wiringEntry{}
	var unreadable []wiringStatus

	for _, path := range []string{codexTarget(workdir, home, false), codexTarget(workdir, home, true)} {
		hooks, err := parseHooksFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				unreadable = append(unreadable, wiringStatus{Status: "unreadable", File: path, Platform: "codex"})
			}
			continue
		}
		for event, arr := range hooks {
			for _, m := range arr {
				for _, hk := range m.Hooks {
					merged[event] = append(merged[event], wiringEntry{Matcher: m.Matcher, Command: hk.Command})
				}
			}
		}
	}

	rows, found := classifyWirings(merged, codexWirings, "codex", selfExe)
	if found == 0 && len(unreadable) == 0 {
		return []wiringStatus{{Platform: "codex", Status: "not wired"}}
	}
	return append(unreadable, rows...)
}

// checkCursorWiring reports wiring status for the Cursor platform. It checks
// the project-level and user-level .cursor/hooks.json files. The same
// collapse rule as checkCodexWiring applies: zero found + no unreadable →
// single "not wired" row.
func checkCursorWiring(workdir, home, selfExe string) []wiringStatus {
	merged := map[string][]wiringEntry{}
	var unreadable []wiringStatus

	for _, path := range []string{cursorTarget(workdir, home, false), cursorTarget(workdir, home, true)} {
		hooks, err := parseCursorHooksFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				unreadable = append(unreadable, wiringStatus{Status: "unreadable", File: path, Platform: "cursor"})
			}
			continue
		}
		for event, arr := range hooks {
			for _, ent := range arr {
				merged[event] = append(merged[event], wiringEntry{Matcher: ent.Matcher, Command: ent.Command})
			}
		}
	}

	rows, found := classifyWirings(merged, cursorWirings, "cursor", selfExe)
	if found == 0 && len(unreadable) == 0 {
		return []wiringStatus{{Platform: "cursor", Status: "not wired"}}
	}
	return append(unreadable, rows...)
}

// firstToken returns the first whitespace-separated token of a command string
// (the binary path).
func firstToken(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// resolvesToExecutable reports whether tok names a runnable binary: an
// executable file path, or — when it has no path separator — a name findable
// in $PATH (hand-wired hooks may use a bare `bashback`; a stat alone would
// misreport those as stale).
func resolvesToExecutable(tok string) bool {
	if tok == "" {
		return false
	}
	if !strings.ContainsRune(tok, os.PathSeparator) {
		_, err := exec.LookPath(tok)
		return err == nil
	}
	return isExecutableFile(tok)
}

// isExecutableFile reports whether path is a regular file with any execute bit.
func isExecutableFile(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !fi.IsDir() && fi.Mode().Perm()&0o111 != 0
}

// claudePluginInstalled reports whether a bashback plugin directory exists under
// home's claude plugin tree. Hooks shipped by the plugin live in the plugin's
// own hooks.json, invisible to the settings scan, so doctor must not report
// them as missing.
func claudePluginInstalled(home string) bool {
	root := filepath.Join(home, ".claude", "plugins")
	found := false
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), "bashback") {
			found = true
			return filepath.SkipAll
		}
		// keep the walk shallow: plugins/<x>/<y>/bashback is the deepest shape
		if rel, rerr := filepath.Rel(root, p); rerr == nil && strings.Count(rel, string(os.PathSeparator)) >= 3 {
			return filepath.SkipDir
		}
		return nil
	})
	return found
}
