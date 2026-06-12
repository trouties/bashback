package journal

import (
	"strings"
	"testing"
)

func TestRedactCommand(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		mustHide  string
		mustKeep  string // substring that should survive
		mustStars bool
	}{
		{"bearer", `curl -H "Authorization: Bearer sk-secret123" https://x`, "sk-secret123", "https://x", true},
		{"token-eq", `deploy --token=abcd1234XYZ`, "abcd1234XYZ", "deploy", true},
		{"password-eq", `mysql --password=hunter2`, "hunter2", "mysql", true},
		{"env-token", `export GITHUB_TOKEN=ghp_aaaabbbbcccc`, "ghp_aaaabbbbcccc", "export", true},
		{"aws-key", `aws configure set x AKIAIOSFODNN7EXAMPLE`, "AKIAIOSFODNN7EXAMPLE", "aws configure", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactCommand(c.in)
			if strings.Contains(got, c.mustHide) {
				t.Errorf("secret leaked: %q still in %q", c.mustHide, got)
			}
			if c.mustStars && !strings.Contains(got, "***") {
				t.Errorf("expected *** marker in %q", got)
			}
			if c.mustKeep != "" && !strings.Contains(got, c.mustKeep) {
				t.Errorf("over-redacted: lost %q from %q", c.mustKeep, got)
			}
		})
	}
}

func TestRedactTruncates(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := RedactCommand(long)
	if len([]rune(got)) != commandMax {
		t.Fatalf("truncated len = %d, want %d", len([]rune(got)), commandMax)
	}
}

func TestRedactLeavesPlainCommandsAlone(t *testing.T) {
	in := "rm -rf build/ && make all"
	if got := RedactCommand(in); got != in {
		t.Fatalf("plain command changed: %q -> %q", in, got)
	}
}

func TestRedactCoversCommonSecretShapes(t *testing.T) {
	// Assembled at runtime so the fixture never trips push-protection scanners.
	glpat := "glpat-" + "AbCdEfGh123456789012"
	cases := []struct{ in, wantGone string }{
		{`export PASSWORD="hunter2-secret"`, "hunter2-secret"},
		{`export PASSWORD='hunter2-secret'`, "hunter2-secret"},
		{`export API_KEY=plainvalue123`, "plainvalue123"},
		{`curl -H "X-Api-Key: sk-live-abc123def456ghi7"`, "sk-live-abc123def456ghi7"},
		{`curl https://user:p4ssw0rd@host/path`, "p4ssw0rd"},
		{`git clone https://ghp_ABCDEFghijkl0123456789ABCDEFghijkl01@github.com/x/y`, "ghp_ABCDEFghijkl0123456789ABCDEFghijkl01"},
		{`slack post --token xoxb-1234567890-abcdefghij`, "xoxb-1234567890-abcdefghij"},
		{`gitlab --token ` + glpat, glpat},
		{`openai --key sk-proj-AbCd1234EfGh5678IjKl`, "sk-proj-AbCd1234EfGh5678IjKl"},
		{`AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" aws s3 ls`, "wJalrXUtnFEMI"},
	}
	for _, c := range cases {
		if got := RedactCommand(c.in); strings.Contains(got, c.wantGone) {
			t.Errorf("RedactCommand(%q) = %q; secret survived", c.in, got)
		}
	}
}

func TestRedactKeepsNonSecrets(t *testing.T) {
	for _, in := range []string{
		`echo hello`, `grep -rn "password" docs/`,
		`git log --grep token`,
	} {
		if got := RedactCommand(in); got != in {
			t.Errorf("RedactCommand(%q) = %q; over-redacted", in, got)
		}
	}
}
