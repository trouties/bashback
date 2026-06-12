package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/skills"
)

func TestSkillStatusThreeStates(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()

	if st, _ := skillStatus(work, home); st != "missing" {
		t.Fatalf("want missing, got %s", st)
	}

	dest := filepath.Join(work, ".claude", "skills", "bashback", "SKILL.md")
	mkdirWrite(t, dest, "old content")
	st, p := skillStatus(work, home)
	if st != "stale" || p != dest {
		t.Fatalf("want stale at %s, got %s %s", dest, st, p)
	}

	if err := os.WriteFile(dest, skills.BashbackSKILL, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := skillStatus(work, home); st != "ok" {
		t.Fatalf("want ok, got %s", st)
	}
}

func TestSkillStatusFindsUserLevel(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	work := t.TempDir()
	dest := filepath.Join(home, ".claude", "skills", "bashback", "SKILL.md")
	mkdirWrite(t, dest, string(skills.BashbackSKILL))
	st, p := skillStatus(work, home)
	if st != "ok" || p != dest {
		t.Fatalf("want ok at user level, got %s %s", st, p)
	}
}

func TestDoctorReportsSkill(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	work := t.TempDir()
	layout := paths.New(filepath.Join(t.TempDir(), ".bashback"))

	var out, errb bytes.Buffer
	Doctor(layout, work, []string{"--json"}, &out, &errb)
	var got struct {
		Skill struct {
			Status string `json:"status"`
			Path   string `json:"path"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("doctor --json unparsable: %v\n%s", err, out.String())
	}
	if got.Skill.Status != "missing" {
		t.Fatalf("want skill missing, got %q", got.Skill.Status)
	}

	mkdirWrite(t, filepath.Join(work, ".claude", "skills", "bashback", "SKILL.md"), string(skills.BashbackSKILL))
	out.Reset()
	Doctor(layout, work, nil, &out, &errb)
	if !strings.Contains(out.String(), "skill: ok") {
		t.Fatalf("doctor text should report skill ok: %q", out.String())
	}
	if strings.Contains(out.String(), "FAIL] skill") {
		t.Fatalf("skill rows are informational, never FAIL: %q", out.String())
	}
}
