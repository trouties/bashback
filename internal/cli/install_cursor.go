package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trouties/bashback/skills"
)

// cursorHookEntry is one entry in a Cursor hooks.json event array.
// Matcher is omitted when empty (sessionStart/sessionEnd have no matcher).
type cursorHookEntry struct {
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
}

var cursorWirings = []expectedWiring{
	{"preToolUse", "Shell", "pre"},
	{"postToolUse", "Shell", "post"},
	{"sessionStart", "", "session-start"},
	{"sessionEnd", "", "session-end"},
}

// cursorTarget resolves the hooks.json to write: ~/.cursor for --user, else
// <workdir>/.cursor/hooks.json. No upward ancestor walk.
func cursorTarget(workdir, home string, user bool) string {
	if user {
		return filepath.Join(home, ".cursor", "hooks.json")
	}
	return filepath.Join(filepath.Clean(workdir), ".cursor", "hooks.json")
}

// mergeCursorWirings applies cursorWirings into hooks. For each wiring it
// rewrites an existing entry whose Command contains " hook <op>" in place
// (updating a stale binary path without duplicating); otherwise it appends.
// Unrelated entries are left untouched.
func mergeCursorWirings(hooks map[string][]cursorHookEntry, exe string, wirings []expectedWiring) {
	for _, w := range wirings {
		cmd := exe + " hook " + w.Op
		needle := " hook " + w.Op
		found := false
		for i := range hooks[w.Event] {
			if strings.Contains(hooks[w.Event][i].Command, needle) {
				hooks[w.Event][i].Matcher = w.Matcher
				hooks[w.Event][i].Command = cmd
				found = true
				break
			}
		}
		if !found {
			hooks[w.Event] = append(hooks[w.Event], cursorHookEntry{
				Matcher: w.Matcher,
				Command: cmd,
			})
		}
	}
}

// installCursor wires bashback's hooks into a Cursor hooks.json and optionally
// writes a .cursor/rules/bashback.mdc teaching artifact.
func installCursor(workdir, home string, user, printOnly, noSkill bool, exe string, stdout, stderr io.Writer) int {
	target := cursorTarget(workdir, home, user)

	top := map[string]json.RawMessage{}
	existed := false
	if b, rerr := os.ReadFile(target); rerr == nil {
		existed = true
		if jerr := json.Unmarshal(b, &top); jerr != nil {
			return errf(stderr, "install: %s is not valid JSON (%v); fix it or run with --print to preview the hooks block", target, jerr)
		}
	}

	// Ensure top-level version is present; default to 1.
	if _, ok := top["version"]; !ok {
		top["version"] = json.RawMessage("1")
	}

	hooks := map[string][]cursorHookEntry{}
	if raw, ok := top["hooks"]; ok {
		if jerr := json.Unmarshal(raw, &hooks); jerr != nil {
			return errf(stderr, "install: the hooks block in %s is malformed (%v); fix it or run with --print to preview", target, jerr)
		}
	}

	mergeCursorWirings(hooks, exe, cursorWirings)

	hb, err := json.Marshal(hooks)
	if err != nil {
		return errf(stderr, "install: encode hooks: %v", err)
	}
	top["hooks"] = json.RawMessage(hb)

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return errf(stderr, "install: encode hooks file: %v", err)
	}
	out = append(out, '\n')

	rulesDest := filepath.Join(filepath.Clean(workdir), ".cursor", "rules", "bashback.mdc")

	if printOnly {
		_, _ = stdout.Write(out)
		if !noSkill {
			fmt.Fprintf(stderr, "skill: not written under --print (target %s)\n", rulesDest)
		}
		return 0
	}

	finish := func() int {
		if !noSkill {
			if user {
				fmt.Fprintln(stdout, "cursor rules: project-level only; --user skips rules")
				return 0
			}
			if err := writeSkillFile(rulesDest, skills.BashbackCursorRules, stdout); err != nil {
				return errf(stderr, "install: hooks wired, rules install failed: %v", err)
			}
		}
		return 0
	}

	// Nothing to do: the file already produces this exact wiring.
	if existed {
		if orig, rerr := os.ReadFile(target); rerr == nil && bytes.Equal(orig, out) {
			fmt.Fprintln(stdout, "already wired")
			return finish()
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return errf(stderr, "install: %v", err)
	}
	// Back up the original before overwriting, but keep the FIRST backup: it is
	// the user's pre-bashback original. A later install would otherwise overwrite
	// it with our own previous output.
	if existed {
		bak := target + ".bashback-bak"
		if _, berr := os.Stat(bak); os.IsNotExist(berr) {
			if orig, rerr := os.ReadFile(target); rerr == nil {
				_ = os.WriteFile(bak, orig, 0o644)
			}
		}
	}
	if err := atomicWriteFile(target, out, 0o644); err != nil {
		return errf(stderr, "install: %v", err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", target)
	return finish()
}
