package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

type churnFileJSON struct {
	Path    string `json:"path"`
	Changes int    `json:"changes"`
}

type statsJSON struct {
	V             int             `json:"v"`
	Total         int             `json:"total"`
	ByStatus      map[string]int  `json:"by_status"`
	CoverageRate  float64         `json:"coverage_rate"`
	Overlapped    int             `json:"overlapped"`
	PreOnly       int             `json:"pre_only"`
	Reclaimed     int             `json:"reclaimed"`
	Sessions      int             `json:"sessions"`
	DiskBytes     int64           `json:"disk_bytes"`
	JournalBytes  int64           `json:"journal_bytes"`
	TopChurnFiles []churnFileJSON `json:"top_churn_files"`
}

// Stats summarizes project health from the journal and on-disk repos.
func Stats(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback stats [--json]")
		fs.PrintDefaults()
	}
	if code, done := parseFS(fs, args, stdout, stderr); done {
		return code
	}
	entries, err := readView(layout, workdir)
	if err != nil {
		return errf(stderr, "read journal: %v", err)
	}

	byStatus := map[string]int{}
	overlapped, preOnly, reclaimed, protectedish := 0, 0, 0, 0
	sessions := map[string]bool{}
	churn := map[string]int{}
	for _, e := range entries {
		label := string(e.Status)
		switch {
		case isPreOnly(e):
			if label == "" {
				label = "pre-only"
			}
		case label == "":
			label = "pending"
		}
		byStatus[label]++
		switch e.Status {
		case journal.StatusProtected, journal.StatusSkippedNoChange, journal.StatusRestored:
			protectedish++
		}
		if e.Overlapped {
			overlapped++
		}
		if isPreOnly(e) {
			preOnly++
		}
		if snapshotsReclaimed(layout, workdir, e) {
			reclaimed++
		}
		if e.SessionID != "" {
			sessions[e.SessionID] = true
		}
		for _, fc := range e.Files {
			churn[fc.P]++
		}
	}
	total := len(entries)
	coverage := 0.0
	if total > 0 {
		coverage = float64(protectedish) / float64(total)
	}
	top := topChurn(churn, 5)
	diskBytes := repoSize(layout.RepoDir(workdir))
	journalBytes := fileSize(layout.JournalPath(workdir))

	if *jsonOut {
		out := statsJSON{
			V: outputVersion, Total: total, ByStatus: byStatus, CoverageRate: coverage,
			Overlapped: overlapped, PreOnly: preOnly, Reclaimed: reclaimed, Sessions: len(sessions),
			DiskBytes: diskBytes, JournalBytes: journalBytes, TopChurnFiles: make([]churnFileJSON, 0, len(top)),
		}
		for _, c := range top {
			out.TopChurnFiles = append(out.TopChurnFiles, churnFileJSON{Path: c.path, Changes: c.count})
		}
		return emitJSON(stdout, stderr, out)
	}

	fmt.Fprintf(stdout, "entries:        %d\n", total)
	statusKeys := make([]string, 0, len(byStatus))
	for k := range byStatus {
		statusKeys = append(statusKeys, k)
	}
	sort.Strings(statusKeys)
	for _, k := range statusKeys {
		fmt.Fprintf(stdout, "  %-20s %d\n", k, byStatus[k])
	}
	fmt.Fprintf(stdout, "coverage:       %.0f%%\n", coverage*100)
	fmt.Fprintf(stdout, "overlapped:     %d\n", overlapped)
	fmt.Fprintf(stdout, "pre-only:       %d\n", preOnly)
	fmt.Fprintf(stdout, "reclaimed:      %d\n", reclaimed)
	fmt.Fprintf(stdout, "sessions:       %d\n", len(sessions))
	fmt.Fprintf(stdout, "disk usage:     %s\n", humanBytes(diskBytes))
	fmt.Fprintf(stdout, "journal size:   %s\n", humanBytes(journalBytes))
	if len(top) > 0 {
		fmt.Fprintln(stdout, "top churn files:")
		for _, c := range top {
			fmt.Fprintf(stdout, "  %-40s %d\n", c.path, c.count)
		}
	}
	return 0
}

type churnEntry struct {
	path  string
	count int
}

// topChurn returns the n most-changed files, ties broken by path for stability.
func topChurn(churn map[string]int, n int) []churnEntry {
	all := make([]churnEntry, 0, len(churn))
	for p, c := range churn {
		all = append(all, churnEntry{p, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].path < all[j].path
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
