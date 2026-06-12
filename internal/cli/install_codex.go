package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trouties/bashback/skills"
)

// codexWirings is the Codex hook set. Codex reads the exact Claude hooks-block
// JSON shape, but its event names differ: there is no PostToolUseFailure, no
// SessionEnd, and no background-task events; Stop is Codex's session-end analog.
// Matchers are regexes against tool_name; anchoring avoids matching e.g. "BashOutput".
var codexWirings = []expectedWiring{
	{"PreToolUse", "^Bash$", "pre"},
	{"PostToolUse", "^Bash$", "post"},
	{"SessionStart", "", "session-start"},
	{"Stop", "", "session-end"},
}

// codexTarget resolves the hooks.json to write: ~/.codex for --user, else the
// project-level <workdir>/.codex/hooks.json. Codex has no upward-walk merge of
// settings the way Claude does, so there is no ancestor search.
func codexTarget(workdir, home string, user bool) string {
	if user {
		return filepath.Join(home, ".codex", "hooks.json")
	}
	return filepath.Join(filepath.Clean(workdir), ".codex", "hooks.json")
}

// installCodex wires bashback's hooks into a Codex hooks.json. Only the target
// file and the wiring table differ from the Claude install.
func installCodex(workdir, home string, user, printOnly, noSkill bool, exe string, stdout, stderr io.Writer) int {
	target := codexTarget(workdir, home, user)
	skillDest := filepath.Join(home, ".agents", "skills", "bashback", "SKILL.md")

	top := map[string]json.RawMessage{}
	existed := false
	if b, rerr := os.ReadFile(target); rerr == nil {
		existed = true
		if jerr := json.Unmarshal(b, &top); jerr != nil {
			return errf(stderr, "install: %s is not valid JSON (%v); fix it or run with --print to preview the hooks block", target, jerr)
		}
	}

	hooks := map[string][]hookMatcher{}
	if raw, ok := top["hooks"]; ok {
		if jerr := json.Unmarshal(raw, &hooks); jerr != nil {
			return errf(stderr, "install: the hooks block in %s is malformed (%v); fix it or run with --print to preview", target, jerr)
		}
	}

	mergeWiringsInto(hooks, exe, codexWirings)

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

	if printOnly {
		_, _ = stdout.Write(out)
		if !noSkill {
			fmt.Fprintf(stderr, "skill: not written under --print (target %s)\n", skillDest)
		}
		return 0
	}

	finish := func() int {
		if !noSkill {
			if err := writeSkillFile(skillDest, skills.BashbackSKILL, stdout); err != nil {
				return errf(stderr, "install: hooks wired, skill install failed: %v", err)
			}
		}
		// Codex does not run newly added hooks until they are trusted, and it does
		// so silently (no prompt). Tell the user to trust them, or protection
		// never fires.
		fmt.Fprintln(stdout, "next: run /hooks in Codex to trust these hooks; until then Codex skips them silently")
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
