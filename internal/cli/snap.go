package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

type snapJSON struct {
	V       int    `json:"v"`
	Key     string `json:"key"`
	PreSHA  string `json:"pre_sha"`
	Note    string `json:"note"`
	Message string `json:"message"`
}

// Snap takes a manual checkpoint of the work-tree. It runs outside
// any session in the shared manual.git, serialized by a flock so concurrent snaps
// don't race index.lock. Recover with `bashback rewind <snap-key>`.
func Snap(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snap", flag.ContinueOnError)
	msg := fs.String("m", "", "checkpoint message")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback snap [-m <message>] [--json]")
		fs.PrintDefaults()
	}
	if pcode, done := parseFS(fs, args, stdout, stderr); done {
		return pcode
	}

	eng := newEngine(layout)
	lock, code := acquireProjectLock(layout, workdir, stderr)
	if lock == nil {
		return code
	}
	defer func() { _ = lock.Release() }()

	entry, err := eng.Snap(ctx(), workdir, *msg)
	if err != nil {
		return errf(stderr, "snap: %v", err)
	}
	if aerr := journal.Append(layout.JournalPath(workdir), entry); aerr != nil {
		return errf(stderr, "snap: journaling failed: %v", aerr)
	}

	if *jsonOut {
		return emitJSON(stdout, stderr, snapJSON{
			V: outputVersion, Key: entry.ToolUseID, PreSHA: entry.PreSHA, Note: entry.Note, Message: entry.Command,
		})
	}
	fmt.Fprintf(stdout, "checkpoint %s\n", entry.ToolUseID)
	if entry.Command != "" {
		fmt.Fprintf(stdout, "message: %s\n", entry.Command)
	}
	if entry.Note != "" {
		fmt.Fprintf(stdout, "note: %s\n", entry.Note)
	}
	fmt.Fprintf(stdout, "restore this checkpoint with: bashback rewind %s\n", entry.ToolUseID)
	return 0
}
