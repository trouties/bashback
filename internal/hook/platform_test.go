package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectPlatform(t *testing.T) {
	cases := []struct{ file, origin, session, command, key, cwd string }{
		{"claude_pre.json", "", "cl-1", "rm -rf x", "tu-1", "/tmp/p"},
		{"codex_pre.json", "codex", "cx-1", "rm -rf x", "call_1", "/tmp/p"},
		{"cursor_pre.json", "cursor", "cv-1", "rm -rf x", "cur-tuid-1", "/tmp/p"},
		{"cursor_topcmd.json", "cursor", "cv-2", "git status", "cur-tuid-2", "/tmp/q"},
	}
	for _, c := range cases {
		b, _ := os.ReadFile(filepath.Join("testdata", c.file))
		p, err := parsePayload(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if p.Origin != c.origin || p.SessionID != c.session || p.ToolInput.Command != c.command || p.ToolUseID != c.key || p.CWD != c.cwd {
			t.Fatalf("%s: normalized %+v", c.file, p)
		}
	}
}

func TestCodexPostToolResponseStringDecodes(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join("testdata", "codex_post.json"))
	if _, err := parsePayload(bytes.NewReader(b)); err != nil {
		t.Fatalf("codex string tool_response must decode, got: %v", err)
	}
}

func TestUnknownPayloadFailsOpen(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join("testdata", "unknown.json"))
	var out, errb bytes.Buffer
	if code := Run("pre", bytes.NewReader(b), &out, &errb); code != 0 {
		t.Fatalf("must exit 0, got %d", code)
	}
}
