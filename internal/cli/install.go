package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"github.com/trouties/bashback/internal/paths"
)

// Install idempotently wires bashback's hooks into a Claude settings.json. Three
// safeguards bound the risk of corrupting a user's settings: a pre-write backup, a
// --print preview, and a merge that leaves unrelated keys intact.
func Install(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	user := fs.Bool("user", false, "write the user-level ~/.claude/settings.json")
	printOnly := fs.Bool("print", false, "print the merged settings to stdout without writing")
	noSkill := fs.Bool("no-skill", false, "skip installing the agent skill")
	codex := fs.Bool("codex", false, "wire Codex CLI hooks (.codex/hooks.json) instead of Claude")
	cursor := fs.Bool("cursor", false, "wire Cursor hooks instead of Claude")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback install [--user] [--print] [--no-skill] [--codex|--cursor]")
		fs.PrintDefaults()
	}
	if code, done := parseFS(fs, args, stdout, stderr); done {
		return code
	}
	// Later manual fs.Usage() calls go to stderr (the buffered -h path is done).
	fs.SetOutput(stderr)

	exe, err := os.Executable()
	if err != nil {
		return errf(stderr, "install: cannot resolve own path: %v", err)
	}
	home, _ := os.UserHomeDir()

	// --codex / --cursor select a different agent; they are mutually exclusive
	// with each other and with the default (Claude) install.
	if *codex && *cursor {
		fs.Usage()
		return errf(stderr, "install: --codex and --cursor are mutually exclusive")
	}
	if *codex {
		return installCodex(workdir, home, *user, *printOnly, *noSkill, exe, stdout, stderr)
	}
	if *cursor {
		return installCursor(workdir, home, *user, *printOnly, *noSkill, exe, stdout, stderr)
	}

	target := installTarget(workdir, home, *user)

	// Refuse to write a user-level settings.json unless --user was given. Check
	// $HOME and the passwd-derived home so an empty/abnormal $HOME cannot let an
	// implicit install silently adopt the real home's global settings.
	if !*user && isUserLevelSettings(target, home) {
		return errf(stderr, "install: refusing to write the user-level %s without --user; re-run with --user to confirm, or run install from a project directory", target)
	}

	// Read the existing settings (or start from an empty object). Top-level keys
	// are kept as RawMessage so unrelated config (permissions, env, …) survives
	// untouched.
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

	mergeWiringsInto(hooks, exe, expectedWirings)

	hb, err := json.Marshal(hooks)
	if err != nil {
		return errf(stderr, "install: encode hooks: %v", err)
	}
	top["hooks"] = json.RawMessage(hb)

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return errf(stderr, "install: encode settings: %v", err)
	}
	out = append(out, '\n')

	if *printOnly {
		_, _ = stdout.Write(out)
		if !*noSkill {
			fmt.Fprintf(stderr, "skill: not written under --print (target %s)\n", skillDest(target))
		}
		return 0
	}

	finish := func() int {
		if !*noSkill {
			if err := installSkill(target, stdout); err != nil {
				return errf(stderr, "install: hooks wired, skill install failed: %v", err)
			}
		}
		printWiringResult(stdout, workdir, home, exe)
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

// mergeWiringsInto updates each expected hook in place if already present (keyed
// on the `hook <op>` suffix, so a stale binary path is rewritten rather than
// duplicated) or appends it otherwise. Unrelated hook entries are left alone.
// The wiring table is a parameter so both the Claude and Codex hook shapes
// (same JSON, different events) share one merge.
func mergeWiringsInto(hooks map[string][]hookMatcher, exe string, wirings []expectedWiring) {
	for _, exp := range wirings {
		cmd := exe + " hook " + exp.Op
		needle := " hook " + exp.Op
		found := false
		for i := range hooks[exp.Event] {
			for j := range hooks[exp.Event][i].Hooks {
				if strings.Contains(hooks[exp.Event][i].Hooks[j].Command, needle) {
					hooks[exp.Event][i].Hooks[j].Command = cmd
					if hooks[exp.Event][i].Hooks[j].Type == "" {
						hooks[exp.Event][i].Hooks[j].Type = "command"
					}
					if hooks[exp.Event][i].Hooks[j].Timeout == 0 {
						hooks[exp.Event][i].Hooks[j].Timeout = 5
					}
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			hooks[exp.Event] = append(hooks[exp.Event], hookMatcher{
				Matcher: exp.Matcher,
				Hooks:   []hookCmd{{Type: "command", Command: cmd, Timeout: 5}},
			})
		}
	}
}

// installTarget resolves the settings.json to write: ~/.claude for --user, else
// the first existing project-level settings.json walking up from workdir
// (settings.local.json is never an install target), falling back to creating
// <workdir>/.claude/settings.json. The walk stops at $HOME: ~/.claude is the
// user-level file, reachable only via --user, so an implicit install must never
// climb into or past home and silently adopt the user's global settings.
// isUserLevelSettings reports whether target is the user-level settings.json of
// either $HOME or the passwd-derived home.
func isUserLevelSettings(target, home string) bool {
	match := func(h string) bool {
		if h == "" {
			return false
		}
		return target == filepath.Join(filepath.Clean(h), ".claude", "settings.json")
	}
	if match(home) {
		return true
	}
	if u, err := osuser.Current(); err == nil {
		return match(u.HomeDir)
	}
	return false
}

func installTarget(workdir, home string, user bool) string {
	if user {
		return filepath.Join(home, ".claude", "settings.json")
	}
	// Stop the walk at any home boundary so an implicit install never climbs into
	// a home directory and silently adopts a user-level ~/.claude. `home` is $HOME
	// (overridable); the passwd-derived home is added as a second boundary so an
	// abnormal $HOME cannot let the walk climb into the real home and adopt its
	// settings without the explicit --user opt-in.
	boundaries := map[string]bool{}
	if home != "" {
		boundaries[filepath.Clean(home)] = true
	}
	if u, err := osuser.Current(); err == nil && u.HomeDir != "" {
		boundaries[filepath.Clean(u.HomeDir)] = true
	}
	dir := filepath.Clean(workdir)
	for !boundaries[dir] {
		p := filepath.Join(dir, ".claude", "settings.json")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(filepath.Clean(workdir), ".claude", "settings.json")
}

// printWiringResult echoes the post-install wiring check so the result is
// what-you-see-is-what-you-get.
func printWiringResult(stdout io.Writer, workdir, home, exe string) {
	for _, w := range checkWiring(workdir, home, exe) {
		mark := "ok  "
		if w.Status != "ok" {
			mark = "FAIL"
		}
		if w.Status == "unreadable" {
			fmt.Fprintf(stdout, "[%s] wiring: settings file unreadable: %s\n", mark, w.File)
			continue
		}
		fmt.Fprintf(stdout, "[%s] wiring %s: %s\n", mark, wiringLabel(w), w.Status)
	}
}
