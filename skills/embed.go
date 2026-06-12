// Package skills embeds the agent-facing skill so every distribution channel
// of the binary (go install, release download, source build) carries it.
package skills

import _ "embed"

//go:embed bashback/SKILL.md
var BashbackSKILL []byte

//go:embed cursor/bashback.mdc
var BashbackCursorRules []byte
