package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trouties/bashback/skills"
)

// skillDest is the skill file paired with a given settings.json: the skills/
// tree under the same .claude directory.
func skillDest(settingsPath string) string {
	return filepath.Join(filepath.Dir(settingsPath), "skills", "bashback", "SKILL.md")
}

// writeSkillFile writes content to dest atomically. No-op when current;
// single-generation backup when the on-disk copy drifted.
func writeSkillFile(dest string, content []byte, stdout io.Writer) error {
	cur, rerr := os.ReadFile(dest)
	if rerr == nil && bytes.Equal(cur, content) {
		fmt.Fprintln(stdout, "skill up to date")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if rerr == nil {
		_ = os.WriteFile(dest+".bashback-bak", cur, 0o644)
	}
	// Atomic replace: a half-written SKILL.md would silently mislead the agent.
	tmp := dest + ".bashback-tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", dest)
	return nil
}

// installSkill writes the embedded skill next to settingsPath.
func installSkill(settingsPath string, stdout io.Writer) error {
	return writeSkillFile(skillDest(settingsPath), skills.BashbackSKILL, stdout)
}

// skillStatus reports the installed skill nearest to workdir: ok (matches the
// embedded copy), stale (drifted), or missing. Search order mirrors
// settingsCandidates: each ancestor's .claude, then ~/.claude.
func skillStatus(workdir, home string) (status, path string) {
	var dirs []string
	dir := filepath.Clean(workdir)
	for {
		dirs = append(dirs, filepath.Join(dir, ".claude"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".claude"), filepath.Join(home, ".agents"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, "skills", "bashback", "SKILL.md")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Equal(b, skills.BashbackSKILL) {
			return "ok", p
		}
		return "stale", p
	}
	return "missing", ""
}
