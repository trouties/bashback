package journal

import "regexp"

// quotedOrBare matches an assignment value: a "double-quoted", a
// 'single-quoted', or a bare token. Quotes are consumed so `K="v"` redacts.
const quotedOrBare = `(?:"[^"]*"|'[^']*'|[^\s"']+)`

// Redaction rules run before a command is stored. The raw command text never
// touches disk. The rule set cannot be exhaustive; residual risk is
// carried by the 0700 tree and documentation.
var redactRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Bearer tokens in any header/flag.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=\-]+`), "${1}***"},
	// Authorization / api-key style header values (key: value or key=value).
	{regexp.MustCompile(`(?i)((?:authorization|x-api-key|api[-_]?key|private-token)\s*[:=]\s*)` + quotedOrBare), "${1}***"},
	// Sensitive name=value assignments (TOKEN=, password=, api_key=, …), quoted or bare.
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:token|secret|password|passwd|api_?key|access_key)[A-Za-z0-9_]*\s*=\s*)` + quotedOrBare), "${1}***"},
	// URL userinfo credentials: scheme://user:pass@host.
	{regexp.MustCompile(`(://[^/\s:@]+:)[^@\s]+(@)`), "${1}***${2}"},
	// Well-known token literals (GitHub, GitLab, Slack, OpenAI-style, AWS id).
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`), "***"},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{16,}\b`), "***"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`), "***"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}\b`), "***"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "***"},
}

// RedactCommand strips known secret shapes and truncates to commandMax runes.
func RedactCommand(cmd string) string {
	for _, r := range redactRules {
		cmd = r.re.ReplaceAllString(cmd, r.repl)
	}
	return truncateRunes(cmd, commandMax)
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
