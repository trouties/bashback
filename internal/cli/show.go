package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

type showJSON struct {
	V            int                  `json:"v"`
	Key          string               `json:"key"`
	Index        int                  `json:"index"`
	SessionID    string               `json:"session_id"`
	TS           string               `json:"ts"`
	DurationMS   int64                `json:"duration_ms"`
	Status       string               `json:"status"`
	Origin       string               `json:"origin,omitempty"`
	Command      string               `json:"command"`
	Note         string               `json:"note"`
	PreSHA       string               `json:"pre_sha"`
	PostSHA      string               `json:"post_sha"`
	Overlapped   bool                 `json:"overlapped"`
	PreOnly      bool                 `json:"pre_only"`
	Reclaimed    bool                 `json:"reclaimed"`
	Background   bool                 `json:"background"`
	Files        []journal.FileChange `json:"files"`
	FilesOmitted int                  `json:"files_omitted"`
	NextSteps    []string             `json:"next_steps"`
}

// Show prints the full archive view of one entry: every journal field, the gate
// flags, the complete changed-files list, and actionable next steps.
func Show(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback show <key> [--json]")
		fs.PrintDefaults()
	}
	rest, code, done := parseFlagsAnywhere(fs, args, stdout, stderr)
	if done {
		return code
	}
	fs.SetOutput(stderr)
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	key := rest[0]
	e, ok, err := resolveEntry(layout, workdir, key)
	if err != nil {
		return errf(stderr, "%v", err)
	}
	if !ok {
		return errf(stderr, "no entry with key %q; see 'bashback list'", key)
	}

	all, _ := readView(layout, workdir)
	idx := atNIndex(all)
	fullKey := journal.DefaultKeyer.Key(e)
	reclaimed := snapshotsReclaimed(layout, workdir, e)
	preOnly := isPreOnly(e)
	background := isBackground(e)
	steps := showNextSteps(fullKey, e, reclaimed, preOnly, bgFinalKeyFor(all, fullKey, background))

	if *jsonOut {
		return emitJSON(stdout, stderr, showJSON{
			V: outputVersion, Key: fullKey, Index: idx[fullKey], SessionID: e.SessionID,
			TS: e.TS, DurationMS: e.DurationMS, Status: string(e.Status), Origin: e.Origin, Command: e.Command,
			Note: e.Note, PreSHA: e.PreSHA, PostSHA: e.PostSHA, Overlapped: e.Overlapped,
			PreOnly: preOnly, Reclaimed: reclaimed, Background: background,
			Files: e.Files, FilesOmitted: e.FilesOmitted, NextSteps: steps,
		})
	}

	fmt.Fprintf(stdout, "key:        %s  (@%d)\n", fullKey, idx[fullKey])
	fmt.Fprintf(stdout, "session:    %s\n", e.SessionID)
	fmt.Fprintf(stdout, "time:       %s\n", e.TS)
	if e.DurationMS > 0 {
		fmt.Fprintf(stdout, "duration:   %d ms\n", e.DurationMS)
	}
	fmt.Fprintf(stdout, "status:     %s\n", listStatus(newReclaimedMemo(layout, workdir), e))
	fmt.Fprintf(stdout, "command:    %s\n", e.Command)
	if e.Origin != "" {
		fmt.Fprintf(stdout, "origin:     %s\n", e.Origin)
	}
	if e.Note != "" {
		fmt.Fprintf(stdout, "note:       %s\n", e.Note)
	}
	fmt.Fprintf(stdout, "pre_sha:    %s\n", e.PreSHA)
	fmt.Fprintf(stdout, "post_sha:   %s\n", e.PostSHA)

	if len(e.Files) > 0 {
		fmt.Fprintf(stdout, "files (%d%s):\n", len(e.Files), omittedSuffix(e.FilesOmitted))
		for _, fc := range e.Files {
			fmt.Fprintf(stdout, "  %s  %s\n", fc.S, fc.P)
		}
	} else if e.FilesOmitted > 0 {
		fmt.Fprintf(stdout, "files:      (all %d omitted)\n", e.FilesOmitted)
	} else {
		fmt.Fprintln(stdout, "files:      (none recorded)")
	}

	fmt.Fprintln(stdout, "next steps:")
	for _, s := range steps {
		fmt.Fprintf(stdout, "  %s\n", s)
	}
	return 0
}

func omittedSuffix(omitted int) string {
	if omitted > 0 {
		return fmt.Sprintf(", +%d omitted", omitted)
	}
	return ""
}

// isBackground reports whether the entry was flagged as a backgrounded command
// (post taken before the command finished; later writes unprotected).
func isBackground(e journal.Entry) bool {
	return strings.Contains(e.Note, "background")
}

// bgFinalPrefix mirrors snapshot.bgFinalPrefix: the synthetic background-
// completion entry key prefix. Kept as a local const so cli does not
// depend on a snapshot internal.
const bgFinalPrefix = "bgfinal_"

// bgFinalKeyFor returns the bgfinal entry key paired with a background entry, if
// one was captured at completion, else "". Only background entries
// can have one.
func bgFinalKeyFor(all []journal.Entry, key string, background bool) string {
	if !background {
		return ""
	}
	cand := bgFinalPrefix + key
	for _, x := range all {
		if journal.DefaultKeyer.Key(x) == cand {
			return cand
		}
	}
	return ""
}

func showNextSteps(key string, e journal.Entry, reclaimed, preOnly bool, bgFinalKey string) []string {
	if reclaimed {
		return []string{"snapshots reclaimed by gc; this is a journal record only (no restore)"}
	}
	var steps []string
	if preOnly {
		// A manual snap is pre-only by design and refuses `restore`; rewind is
		// its only recovery verb, so don't offer the interrupted-command path.
		if e.Status == journal.StatusManual {
			return append(steps, fmt.Sprintf("rewind whole tree to this checkpoint: bashback rewind %s", key))
		}
		steps = append(steps,
			fmt.Sprintf("undo (against current work-tree): bashback restore %s --force", key),
			fmt.Sprintf("rewind whole tree to this point:  bashback rewind %s", key))
		return steps
	}
	steps = append(steps, fmt.Sprintf("inspect changes: bashback diff %s", key))
	restoreCmd := fmt.Sprintf("undo this command:  bashback restore %s", key)
	if e.Overlapped {
		restoreCmd += " --force"
	}
	steps = append(steps, restoreCmd)
	steps = append(steps, fmt.Sprintf("rewind whole tree: bashback rewind %s", key))
	if bgFinalKey != "" {
		steps = append(steps, fmt.Sprintf("inspect background writes: bashback diff %s", bgFinalKey))
	}
	return steps
}
