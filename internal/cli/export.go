package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/trouties/bashback/internal/paths"
)

// Export writes an entry's pre..post change as a binary-safe patch that `git apply`
// can replay (binary files included, unlike `diff`'s display patch). It streams to
// stdout by default or to --out; there is no --json form, the patch being the
// machine format.
func Export(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "write the patch to a file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback export <key> [--out <file>] [path...]")
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
	if snapshotsReclaimed(layout, workdir, e) {
		return errf(stderr, "snapshots for %q were reclaimed by gc; nothing to export", key)
	}
	if e.PostSHA == "" {
		// Pre-only/interrupted or a manual checkpoint: no post snapshot to bound the
		// patch, so rewind is the recovery verb, not export.
		return errf(stderr, "%q has no post snapshot to export (pre-only/interrupted or a manual checkpoint); to recover its tree use `bashback rewind %s`", key, key)
	}
	if e.PreSHA == "" {
		return errf(stderr, "entry %q is incomplete (no pre/post range)", key)
	}

	r := newEngine(layout).RepoFor(workdir, e.SessionID)
	patch, err := r.DiffPatchBinary(ctx(), e.PreSHA, e.PostSHA, rest[1:])
	if err != nil {
		return errf(stderr, "export: %v", err)
	}

	if *out != "" {
		if werr := os.WriteFile(*out, patch, 0o644); werr != nil {
			return errf(stderr, "write %s: %v", *out, werr)
		}
		fmt.Fprintf(stderr, "wrote %d bytes to %s\n", len(patch), *out)
		return 0
	}
	_, _ = stdout.Write(patch)
	return 0
}
