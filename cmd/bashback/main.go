// Command bashback is the single binary behind the hook client, the per-session
// snapshot daemon, and the user-facing CLI. main only dispatches subcommands.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// commandSet is the v0.1 CLI surface. Subcommands not yet wired to
// their implementation return a non-zero "not implemented" exit so callers and
// tests can tell a known-but-pending command from an unknown one.
var topLevel = map[string]bool{
	"hook":    true,
	"daemon":  true,
	"list":    true,
	"diff":    true,
	"show":    true,
	"log":     true,
	"export":  true,
	"stats":   true,
	"restore": true,
	"undo":    true,
	"rewind":  true,
	"snap":    true,
	"gc":      true,
	"doctor":  true,
	"config":  true,
	"install": true,
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "version", "--version":
		if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
			fmt.Fprintln(stderr, "usage: bashback version")
			return 2
		}
		fmt.Fprintln(stdout, versionString())
		return 0
	}
	cmd := args[0]
	if !topLevel[cmd] {
		fmt.Fprintf(stderr, "bashback: unknown subcommand %q\n", cmd)
		usage(stderr)
		return 2
	}
	return dispatch(cmd, args[1:], stdin, stdout, stderr)
}

// version is set by goreleaser via -ldflags at release build time. When empty
// (go install / source build) we fall back to the module's embedded VCS info.
var version string

// versionString reports the build version: the release ldflags value when
// present, else the embedded VCS info. A plain `go build` yields "(devel)";
// that fallback is the honest answer, not an error.
func versionString() string {
	if version != "" {
		return "bashback " + version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "bashback (devel)"
	}
	return "bashback " + info.Main.Version
}

func usage(w io.Writer) {
	fmt.Fprint(w, `bashback — command-level snapshot & undo for bash side effects

usage: bashback <command> [args]

commands:
  hook pre|post|bg|session-start|session-end   agent hook entrypoints (fail-open)
  daemon run                                per-session snapshot daemon (internal)
  list                                      list snapshots from the journal
  diff <key> [--stat]                       show changes a command made
  stats                                     project health summary
  show <key>                                full detail view of one entry
  log <path>                                history of commands that touched a path
  export <key> [--out <file>]               write an entry's change as a git-apply patch
  restore <key> [--3way] [--force] [path…]  undo a command's file side effects
  undo [--3way] [--dry-run]                 undo the most recent file-changing command
  rewind <key> [--force] [--dry-run]        restore the whole tree to a command's pre state
  snap [-m <message>]                       take a manual whole-tree checkpoint
  gc [--older-than <dur>] [--all] [--dry-run]  reclaim expired session repos
  doctor                                    environment self-check
  install [--user] [--print] [--no-skill] [--codex|--cursor]   wire hooks into Claude/Codex/Cursor
  config list|get|set|unset                 read/write per-project settings
  version                                   print the build version
  help                                      print this message

Run "bashback <command> -h" for a command's own flags.
`)
}
