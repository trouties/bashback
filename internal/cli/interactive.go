package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/trouties/bashback/internal/snapshot"
)

// restoreInput is the input source for interactive restore (-p); a package var so
// tests can script the answers without a pty.
var restoreInput io.Reader = os.Stdin

// isInteractive reports whether both stdin and stdout are character devices (a
// real terminal). A package var so tests can force the answer. Interactive
// restore refuses a non-TTY rather than blocking on a pipe.
var isInteractive = func() bool {
	si, serr := os.Stdin.Stat()
	so, oerr := os.Stdout.Stat()
	return serr == nil && oerr == nil &&
		si.Mode()&os.ModeCharDevice != 0 && so.Mode()&os.ModeCharDevice != 0
}

const hunkHelp = "y - revert this hunk; n - keep it; a - revert this and all later hunks in the file; " +
	"d - keep this and all later hunks in the file; q - quit, reverting nothing; ? - help"

// runInteractive walks the patch file-by-file, asking which changes to revert, and
// returns the assembled selection. Routing by plan: command-created paths get a
// delete prompt, contentless/binary files a whole-file revert prompt, text files
// hunk-by-hunk. aborted is true when the user quit; the caller then makes no changes.
func runInteractive(files []patchFile, plan snapshot.RestorePlan, in io.Reader, out io.Writer) (sel snapshot.PartialSelection, aborted bool) {
	deleteSet := map[string]bool{}
	for _, p := range plan.Delete {
		deleteSet[p] = true
	}
	reader := bufio.NewReader(in)
	picked := map[[2]int]bool{}

	for fi, f := range files {
		switch {
		case deleteSet[f.Path]:
			// A command-created file is reverted by removing it whole.
			switch readChoice(reader, out, fmt.Sprintf("delete %s?", f.Path), "[y/n]") {
			case 'y':
				sel.Delete = append(sel.Delete, f.Path)
			case 'q':
				return sel, true
			}
		case len(f.Hunks) == 0:
			// Binary or otherwise non-textual change: revert the whole file from pre.
			switch readChoice(reader, out, fmt.Sprintf("revert %s?", f.Path), "[y/n]") {
			case 'y':
				sel.Checkout = append(sel.Checkout, f.Path)
			case 'q':
				return sel, true
			}
		default:
			mode := byte(0) // 'a' = take rest of file, 'd' = drop rest of file
			for hi := range f.Hunks {
				if mode == 'a' {
					picked[[2]int{fi, hi}] = true
					continue
				}
				if mode == 'd' {
					continue
				}
				fmt.Fprint(out, f.Hunks[hi])
				switch readChoice(reader, out, fmt.Sprintf("revert this hunk in %s?", f.Path), "[y,n,a,d,q,?]") {
				case 'y':
					picked[[2]int{fi, hi}] = true
				case 'n':
					// keep this hunk
				case 'a':
					picked[[2]int{fi, hi}] = true
					mode = 'a'
				case 'd':
					mode = 'd'
				case 'q':
					return sel, true
				}
			}
		}
	}

	sel.Patch = []byte(assemblePatch(files, func(fi, hi int) bool { return picked[[2]int{fi, hi}] }))
	return sel, false
}

// readChoice prints "prompt choices " and returns the first character of the
// next non-empty input line. "?" prints help and re-asks; end-of-input is treated
// as quit so a truncated script never hangs or silently applies.
func readChoice(reader *bufio.Reader, out io.Writer, prompt, choices string) byte {
	for {
		fmt.Fprintf(out, "%s %s ", prompt, choices)
		line, err := reader.ReadString('\n')
		s := strings.TrimSpace(line)
		if s == "" {
			if err != nil {
				return 'q'
			}
			continue
		}
		if s[0] == '?' {
			fmt.Fprintln(out, hunkHelp)
			continue
		}
		return s[0]
	}
}
