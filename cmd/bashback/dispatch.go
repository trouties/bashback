package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/trouties/bashback/internal/cli"
	"github.com/trouties/bashback/internal/daemon"
	"github.com/trouties/bashback/internal/hook"
	"github.com/trouties/bashback/internal/paths"
)

// dispatch routes a known top-level subcommand to its handler.
func dispatch(cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch cmd {
	case "hook":
		return runHook(args, stdin, stdout, stderr)
	case "daemon":
		return runDaemon(args, stderr)
	case "list":
		return userCmd(cli.List, args, stdout, stderr)
	case "diff":
		return userCmd(cli.Diff, args, stdout, stderr)
	case "show":
		return userCmd(cli.Show, args, stdout, stderr)
	case "log":
		return userCmd(cli.Log, args, stdout, stderr)
	case "export":
		return userCmd(cli.Export, args, stdout, stderr)
	case "stats":
		return userCmd(cli.Stats, args, stdout, stderr)
	case "restore":
		return userCmd(cli.Restore, args, stdout, stderr)
	case "undo":
		return userCmd(cli.Undo, args, stdout, stderr)
	case "rewind":
		return userCmd(cli.Rewind, args, stdout, stderr)
	case "snap":
		return userCmd(cli.Snap, args, stdout, stderr)
	case "gc":
		return userCmd(cli.GC, args, stdout, stderr)
	case "doctor":
		return userCmd(cli.Doctor, args, stdout, stderr)
	case "config":
		return userCmd(cli.Config, args, stdout, stderr)
	case "install":
		return userCmd(cli.Install, args, stdout, stderr)
	default:
		return notImplemented(stderr, cmd)
	}
}

// userCmd resolves the layout and current project dir, then runs a CLI handler.
func userCmd(fn func(paths.Layout, string, []string, io.Writer, io.Writer) int, args []string, stdout, stderr io.Writer) int {
	layout, err := paths.Default()
	if err != nil {
		fmt.Fprintf(stderr, "bashback: %v\n", err)
		return 1
	}
	workdir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "bashback: %v\n", err)
		return 1
	}
	return fn(layout, workdir, args, stdout, stderr)
}

// runHook dispatches the hook subcommands. It always returns 0 (fail-open).
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "bashback hook: missing op (pre|post|bg|session-start|session-end)")
		return 0 // fail-open even on misuse
	}
	return hook.Run(args[0], stdin, stdout, stderr)
}

// runDaemon handles `daemon run --session <id>` (internal, client-spawned).
func runDaemon(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "usage: bashback daemon run --session <id>")
		return 2
	}
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "session id (socket namespace)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *session == "" {
		fmt.Fprintln(stderr, "bashback daemon run: --session is required")
		return 2
	}
	layout, err := paths.Default()
	if err != nil {
		fmt.Fprintf(stderr, "bashback daemon run: %v\n", err)
		return 1
	}
	logger, closeLog := daemonLogger(layout, stderr)
	defer closeLog()

	if err := daemon.Run(layout, *session, logger); err != nil {
		if err == daemon.ErrAlreadyRunning {
			return 0 // another daemon owns the socket; benign
		}
		fmt.Fprintf(stderr, "bashback daemon run: %v\n", err)
		return 1
	}
	return 0
}

func notImplemented(stderr io.Writer, name string) int {
	fmt.Fprintf(stderr, "bashback: %s is not implemented yet\n", name)
	return 1
}

func daemonLogger(layout paths.Layout, stderr io.Writer) (*log.Logger, func()) {
	w, err := daemon.NewRotatingWriter(layout.LogPath(), 8<<20)
	if err != nil {
		return log.New(stderr, "bashback ", log.LstdFlags), func() {}
	}
	return log.New(w, "bashback ", log.LstdFlags), func() { _ = w.Close() }
}
