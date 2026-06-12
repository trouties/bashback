package skills

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedSkillLooksValid(t *testing.T) {
	if !bytes.HasPrefix(BashbackSKILL, []byte("---\nname: bashback\n")) {
		t.Fatalf("embedded skill missing frontmatter: %.60q", BashbackSKILL)
	}
	if len(BashbackSKILL) < 1024 {
		t.Fatalf("embedded skill suspiciously small: %d bytes", len(BashbackSKILL))
	}
}

func TestCursorRulesMirrorsSkill(t *testing.T) {
	skillBody := stripFrontmatter(t, BashbackSKILL)
	rulesBody := stripFrontmatter(t, BashbackCursorRules)
	if !bytes.Equal(skillBody, rulesBody) {
		t.Fatal("cursor rules body must equal SKILL.md body; regenerate .cursor/rules/bashback.mdc")
	}
	if !bytes.HasPrefix(BashbackCursorRules, []byte("---\ndescription:")) {
		t.Fatalf("mdc frontmatter malformed: %.80q", BashbackCursorRules)
	}
	panel, err := os.ReadFile("../.cursor/rules/bashback.mdc")
	if err != nil {
		t.Fatalf("panel file missing: %v", err)
	}
	if !bytes.Equal(panel, BashbackCursorRules) {
		t.Fatal(".cursor/rules/bashback.mdc must be a verbatim copy of skills/cursor/bashback.mdc")
	}
}

func stripFrontmatter(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("empty input")
	}
	if !bytes.HasPrefix(b, []byte("---\n")) {
		t.Fatalf("no opening frontmatter fence: %.80q", b)
	}
	rest := b[4:]
	idx := bytes.Index(rest, []byte("\n---\n"))
	if idx == -1 {
		t.Fatal("no closing frontmatter fence")
	}
	return rest[idx+5:]
}
