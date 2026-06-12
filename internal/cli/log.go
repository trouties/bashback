package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

type logEntryJSON struct {
	Key     string `json:"key"`
	Index   int    `json:"index"`
	TS      string `json:"ts"`
	Change  string `json:"change"`
	Origin  string `json:"origin,omitempty"`
	Command string `json:"command"`
	BgOf    string `json:"bg_of,omitempty"`
}

// bgOf recovers the original command's key from a bgfinal entry's key by
// stripping the bgfinal_ marker, or returns "" for an ordinary key. A
// background final snapshot is keyed bgfinal_<original key>, so this is exact.
func bgOf(key string) string {
	if rest := strings.TrimPrefix(key, "bgfinal_"); rest != key {
		return rest
	}
	return ""
}

type logJSON struct {
	V                int            `json:"v"`
	Path             string         `json:"path"`
	Entries          []logEntryJSON `json:"entries"`
	OlderWithoutData int            `json:"older_without_file_data"`
}

// Log shows the history of one path: which commands touched it. It
// reads only the journal's files field, so it works after the snapshots are GC'd.
// Matching is exact or directory-prefix. Entries predating the files field cannot
// match and are counted in a trailing note.
func Log(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	n := fs.Int("n", 0, "show only the latest N entries (0 = all)")
	since := fs.String("since", "", "only entries newer than this (2h, 3d, or RFC3339)")
	full := fs.Bool("full", false, "do not truncate commands")
	abs := fs.Bool("abs", false, "show absolute RFC3339 timestamps instead of relative")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback log <path> [-n N] [--since <dur|RFC3339>] [--full] [--abs] [--json]")
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
	path := strings.TrimSuffix(rest[0], "/")

	all, err := readView(layout, workdir)
	if err != nil {
		return errf(stderr, "read journal: %v", err)
	}
	atN := atNIndex(all)
	short := shortKeys(all)
	now := time.Now()

	var cutoff time.Time
	if *since != "" {
		c, perr := parseSince(*since, now)
		if perr != nil {
			return errf(stderr, "%v", perr)
		}
		cutoff = c
	}

	type row struct {
		e      journal.Entry
		change string
	}
	var rows []row
	olderWithoutData := 0
	for _, e := range all {
		if len(e.Files) == 0 {
			if entryChangedButHasNoFileData(e) {
				olderWithoutData++
			}
			continue
		}
		if change := matchChange(e.Files, path); change != "" {
			if *since != "" {
				t, perr := time.Parse(time.RFC3339, e.TS)
				if perr != nil || t.Before(cutoff) {
					continue
				}
			}
			rows = append(rows, row{e, change})
		}
	}
	// -n keeps the latest N matching rows (file order is old→new), like list.
	if *n > 0 && len(rows) > *n {
		rows = rows[len(rows)-*n:]
	}

	if *jsonOut {
		// Newest-first, so a parsing agent reads the latest touch at entries[0]
		// (matching @1). The text view below stays in file order (old->new).
		out := logJSON{V: outputVersion, Path: path, Entries: make([]logEntryJSON, 0, len(rows)), OlderWithoutData: olderWithoutData}
		for i := len(rows) - 1; i >= 0; i-- {
			r := rows[i]
			key := journal.DefaultKeyer.Key(r.e)
			out.Entries = append(out.Entries, logEntryJSON{
				Key: key, Index: atN[key], TS: r.e.TS, Change: r.change, Origin: r.e.Origin, Command: r.e.Command, BgOf: bgOf(key),
			})
		}
		return emitJSON(stdout, stderr, out)
	}

	if len(rows) == 0 {
		fmt.Fprintf(stdout, "no recorded changes to %q\n", path)
	} else {
		fmt.Fprintf(stdout, "%-5s  %-26s  %-12s  %-7s  %s\n", "@N", "KEY", "TIME", "CHANGE", "COMMAND")
		for _, r := range rows {
			key := journal.DefaultKeyer.Key(r.e)
			ts := humanAge(r.e.TS, now)
			if *abs {
				ts = r.e.TS
			}
			cmd := r.e.Command
			if !*full {
				cmd = truncate(cmd, 60)
			}
			// Attribute a background final to the command that spawned it, using the
			// original's short key (full when it is no longer in view).
			if orig := bgOf(key); orig != "" {
				ref := orig
				if s, ok := short[orig]; ok {
					ref = s
				}
				cmd = fmt.Sprintf("(bg of %s) %s", ref, cmd)
			}
			fmt.Fprintf(stdout, "%-5s  %-26s  %-12s  %-7s  %s\n",
				fmt.Sprintf("@%d", atN[key]), keyCol(short, key, *full), ts, r.change, cmd)
		}
	}
	if olderWithoutData > 0 {
		fmt.Fprintf(stdout, "(%d older entr%s have no file data)\n", olderWithoutData, plural(olderWithoutData, "y", "ies"))
	}
	return 0
}

// matchChange returns the change letter(s) for the path in this entry's files, or
// "" if it does not touch the path. Exact file match or directory prefix; for a
// directory the distinct letters are joined (e.g. "A,M").
func matchChange(files []journal.FileChange, path string) string {
	seen := map[string]bool{}
	var letters []string
	for _, f := range files {
		if f.P == path || strings.HasPrefix(f.P, path+"/") {
			if !seen[f.S] {
				seen[f.S] = true
				letters = append(letters, f.S)
			}
		}
	}
	sort.Strings(letters)
	return strings.Join(letters, ",")
}

// entryChangedButHasNoFileData reports an entry that recorded a real change but
// predates the files field (so it cannot be matched by path).
func entryChangedButHasNoFileData(e journal.Entry) bool {
	if len(e.Files) > 0 {
		return false
	}
	if e.PreSHA == "" || e.PostSHA == "" || e.PreSHA == e.PostSHA {
		return false
	}
	return e.Status == journal.StatusProtected || e.Status == journal.StatusRestored
}
