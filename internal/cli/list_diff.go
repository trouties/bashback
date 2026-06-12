package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
)

// listEntryJSON is one machine-readable journal row: raw status with the
// read-time flags as booleans and the full, untruncated command.
type listEntryJSON struct {
	Key          string `json:"key"`
	Index        int    `json:"index"`
	SessionID    string `json:"session_id"`
	TS           string `json:"ts"`
	Status       string `json:"status"`
	Origin       string `json:"origin,omitempty"`
	Command      string `json:"command"`
	Overlapped   bool   `json:"overlapped"`
	PreOnly      bool   `json:"pre_only"`
	Reclaimed    bool   `json:"reclaimed"`
	Background   bool   `json:"background"`
	Pending      bool   `json:"pending"`
	Files        int    `json:"files"`
	FilesOmitted int    `json:"files_omitted,omitempty"`
}

type listJSON struct {
	V            int             `json:"v"`
	ProtectPaths []string        `json:"protect_paths,omitempty"`
	Entries      []listEntryJSON `json:"entries"`
}

// List prints journal entries for the current project with filtering and display
// options. GC'd snapshots are flagged; unpaired half-rows show as
// pending. Display order stays file order (old→new); @N is the ts-descending
// address.
func List(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	n := fs.Int("n", -1, "latest N entries (default 50 in text mode; 0 = all)")
	var statuses stringSliceFlag
	fs.Var(&statuses, "status", "filter by status (repeatable)")
	since := fs.String("since", "", "only entries newer than this (2h, 3d, or RFC3339)")
	session := fs.String("session", "", "filter by session id prefix")
	grep := fs.String("grep", "", "filter by command substring")
	full := fs.Bool("full", false, "do not truncate commands")
	abs := fs.Bool("abs", false, "show absolute RFC3339 timestamps instead of relative")
	bySession := fs.Bool("by-session", false, "group entries by session")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	if code, done := parseFS(fs, args, stdout, stderr); done {
		return code
	}

	all, err := readView(layout, workdir)
	if err != nil {
		return errf(stderr, "read journal: %v", err)
	}
	protect := config.Load(layout, workdir, config.OSEnv()).ProtectPaths
	now := time.Now()

	var cutoff time.Time
	if *since != "" {
		c, perr := parseSince(*since, now)
		if perr != nil {
			return errf(stderr, "%v", perr)
		}
		cutoff = c
	}

	// @N indexes the full view, so addressing is stable under filters.
	atN := atNIndex(all)

	filtered := make([]journal.Entry, 0, len(all))
	for _, e := range all {
		if !statusMatches(e, statuses) {
			continue
		}
		if *session != "" && !strings.HasPrefix(e.SessionID, *session) {
			continue
		}
		if *grep != "" && !strings.Contains(e.Command, *grep) {
			continue
		}
		if *since != "" {
			t, perr := time.Parse(time.RFC3339, e.TS)
			if perr != nil || t.Before(cutoff) {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	if *n > 0 && len(filtered) > *n {
		filtered = filtered[len(filtered)-*n:]
	}

	rec := newReclaimedMemo(layout, workdir)
	if *jsonOut {
		out := listJSON{V: outputVersion, ProtectPaths: protect, Entries: make([]listEntryJSON, 0, len(filtered))}
		for _, e := range filtered {
			key := journal.DefaultKeyer.Key(e)
			out.Entries = append(out.Entries, listEntryJSON{
				Key:          key,
				Index:        atN[key],
				SessionID:    e.SessionID,
				TS:           e.TS,
				Status:       string(e.Status),
				Origin:       e.Origin,
				Command:      e.Command,
				Overlapped:   e.Overlapped,
				PreOnly:      isPreOnly(e),
				Reclaimed:    rec.reclaimed(e),
				Background:   isBackground(e),
				Pending:      e.Status == "" && !isPreOnly(e),
				Files:        len(e.Files),
				FilesOmitted: e.FilesOmitted,
			})
		}
		return emitJSON(stdout, stderr, out)
	}

	if len(filtered) == 0 {
		if len(all) == 0 {
			fmt.Fprintln(stdout, "no snapshots recorded for this project")
			if parent := nearestProtectedParent(layout, workdir); parent != "" {
				fmt.Fprintf(stdout, "note: snapshots exist for parent %s; run bashback from there\n", parent)
			} else {
				fmt.Fprintln(stdout, "if you expected entries here, run `bashback doctor` to verify hook wiring")
			}
		} else {
			fmt.Fprintln(stdout, "no entries match the given filters")
		}
		return 0
	}
	if len(protect) > 0 {
		fmt.Fprintf(stdout, "sparse protection active: only %s is snapshotted\n", strings.Join(protect, ", "))
	}
	// Default (no -n) bounds the text view to the latest window so a busy project
	// doesn't flood the terminal; -n 0 forces the full list, -n N a custom size.
	shown := filtered
	if *n < 0 && len(filtered) > listDefaultWindow {
		shown = filtered[len(filtered)-listDefaultWindow:]
	}
	// Short keys are computed over the full view so the displayed KEY is unique
	// across everything addressable, not just the filtered slice.
	short := shortKeys(all)
	fmt.Fprintf(stdout, "%-5s  %-26s  %-12s  %-24s  %5s  %s\n", "@N", "KEY", "TIME", "STATUS", "FILES", "COMMAND")
	if *bySession {
		return listBySession(stdout, rec, shown, atN, short, now, *full, *abs)
	}
	for _, e := range shown {
		printListRow(stdout, rec, e, atN, short, now, *full, *abs)
	}
	if len(shown) < len(filtered) {
		fmt.Fprintf(stdout, "(+%d older entries; use -n 0 for all)\n", len(filtered)-len(shown))
	}
	return 0
}

// listDefaultWindow bounds the text list when no -n is given, so a long history
// doesn't flood the terminal while -n 0 still shows everything.
const listDefaultWindow = 50

// keyCol renders the KEY column: the full key under --full, otherwise the
// shortest-unique abbreviation. No truncation — a collision-extended
// short key simply overflows the column rather than losing characters, so the
// displayed key always resolves.
func keyCol(short map[string]string, key string, full bool) string {
	if full {
		return key
	}
	if s, ok := short[key]; ok {
		return s
	}
	return key
}

// printListRow renders one journal entry as a list row (shared by the flat and
// the by-session views).
func printListRow(stdout io.Writer, rec *reclaimedMemo, e journal.Entry, atN map[string]int, short map[string]string, now time.Time, full, abs bool) {
	key := journal.DefaultKeyer.Key(e)
	ts := humanAge(e.TS, now)
	if abs {
		ts = e.TS
	}
	cmd := e.Command
	if !full {
		cmd = truncate(cmd, 60)
	}
	fmt.Fprintf(stdout, "%-5s  %-26s  %-12s  %-24s  %5s  %s\n",
		fmt.Sprintf("@%d", atN[key]), keyCol(short, key, full), ts, truncate(listStatus(rec, e), 24), filesCol(e), cmd)
}

// listBySession renders entries grouped by session in first-seen order, keeping
// file order within each group. Each group header shows the short id,
// the group's earliest/latest entry time, and a count, tagged [live] when a
// daemon listener is up and [manual] when the group holds only manual snapshots.
func listBySession(stdout io.Writer, rec *reclaimedMemo, filtered []journal.Entry, atN map[string]int, short map[string]string, now time.Time, full, abs bool) int {
	live := liveSessions(rec.layout)
	var order []string
	groups := map[string][]journal.Entry{}
	for _, e := range filtered {
		if _, ok := groups[e.SessionID]; !ok {
			order = append(order, e.SessionID)
		}
		groups[e.SessionID] = append(groups[e.SessionID], e)
	}
	for gi, sid := range order {
		if gi > 0 {
			fmt.Fprintln(stdout)
		}
		g := groups[sid]
		started, last := groupSpan(g)
		startedStr, lastStr := humanAge(started, now), humanAge(last, now)
		if abs {
			startedStr, lastStr = started, last
		}
		header := fmt.Sprintf("session %s  started %s  last %s  %d %s",
			shortSession(sid), startedStr, lastStr, len(g), plural(len(g), "entry", "entries"))
		if live[sid] {
			header += " [live]"
		}
		if allManual(g) {
			header += " [manual]"
		}
		fmt.Fprintln(stdout, header)
		for _, e := range g {
			printListRow(stdout, rec, e, atN, short, now, full, abs)
		}
	}
	return 0
}

// groupSpan returns the earliest and latest entry timestamp in a group. RFC3339
// UTC timestamps compare lexicographically, so no parsing is needed.
func groupSpan(g []journal.Entry) (started, last string) {
	for _, e := range g {
		if started == "" || e.TS < started {
			started = e.TS
		}
		if last == "" || e.TS > last {
			last = e.TS
		}
	}
	return started, last
}

// allManual reports whether every entry in a non-empty group is a manual snapshot.
func allManual(g []journal.Entry) bool {
	for _, e := range g {
		if e.Status != journal.StatusManual {
			return false
		}
	}
	return len(g) > 0
}

// listStatus renders the decorated status string with the standard annotations.
func listStatus(rec *reclaimedMemo, e journal.Entry) string {
	status := string(e.Status)
	switch {
	case isPreOnly(e):
		if status == "" {
			status = "pre-only"
		}
		// A manual snap is pre-only by design (it has no post side), so the
		// interrupted-command annotation would misread it. Its `manual` status
		// already conveys the intent.
		if e.Status != journal.StatusManual {
			status += " (interrupted?)"
		}
	case status == "":
		status = "pending"
	}
	if e.Overlapped {
		status += " (overlap)"
	}
	if isBackground(e) {
		status += " (background)"
	}
	if e.Origin != "" {
		status += " (" + e.Origin + ")"
	}
	if rec.reclaimed(e) {
		status += " [reclaimed]"
	}
	return status
}

// filesCol renders the changed-files count, with a + when entries were omitted.
func filesCol(e journal.Entry) string {
	if len(e.Files) == 0 && e.FilesOmitted == 0 {
		return ""
	}
	if e.FilesOmitted > 0 {
		return fmt.Sprintf("%d+", len(e.Files))
	}
	return strconv.Itoa(len(e.Files))
}

// statusMatches reports whether an entry passes the (possibly empty) status
// filter. An empty filter passes everything; "pre-only" matches orphan pres.
func statusMatches(e journal.Entry, want stringSliceFlag) bool {
	if len(want) == 0 {
		return true
	}
	for _, s := range want {
		if s == string(e.Status) {
			return true
		}
		if s == "pre-only" && isPreOnly(e) {
			return true
		}
	}
	return false
}

// Diff shows the changes a command made (pre..post), flagging overlapped and
// reclaimed entries. --stat shows a name-status + numstat summary
// instead of the full patch.
func Diff(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	stat := fs.Bool("stat", false, "show a name-status + numstat summary instead of the patch")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback diff <key> [<key2>] [--stat] [--json] [path...]")
		fs.PrintDefaults()
	}
	parsed, code, done := parseFlagsAnywhere(fs, args, stdout, stderr)
	if done {
		return code
	}
	args = parsed
	if len(args) == 0 {
		return errf(stderr, "usage: bashback diff <key> [<key2>] [--stat] [path...]")
	}
	entries, rerr := readView(layout, workdir)
	if rerr != nil {
		return errf(stderr, "read journal: %v", rerr)
	}
	key := args[0]
	e, ok, err := resolveEntryIn(entries, key)
	if err != nil {
		return errf(stderr, "%v", err)
	}
	if !ok {
		return errf(stderr, "no entry with key %q; see 'bashback list'", key)
	}

	// Cross-entry diff: if the second positional resolves to an entry, diff the
	// two tree states. An ambiguous second arg is surfaced as an error; a clean
	// miss — including a fragment under the key-prefix floor — falls through to
	// single-key path-filter mode (a path that is not a key). Entry resolution
	// wins on ambiguity, by design.
	if len(args) >= 2 {
		e2, ok2, err2 := resolveEntryIn(entries, args[1])
		var tooShort tooShortError
		if err2 != nil && !errors.As(err2, &tooShort) {
			return errf(stderr, "%v", err2)
		}
		if ok2 {
			return diffTwoEntries(layout, workdir, e, e2, args[2:], *stat, *jsonOut, stdout, stderr)
		}
	}

	if e.Overlapped {
		fmt.Fprintln(stderr, "WARNING: this entry is overlapped — diff attribution may include other concurrent commands")
	}
	if snapshotsReclaimed(layout, workdir, e) {
		return errf(stderr, "snapshots for %q were reclaimed by gc; nothing to diff", key)
	}
	if isPreOnly(e) {
		if e.Status == journal.StatusManual {
			return errf(stderr, "%q is a manual checkpoint (pre-only by design); to roll the tree back to it use `bashback rewind %s`", key, key)
		}
		return errf(stderr, "%q is pre-only (interrupted; no post snapshot); to undo use `bashback restore %s --force` (compares against the current work-tree)", key, key)
	}
	if e.PreSHA == "" || e.PostSHA == "" {
		return errf(stderr, "entry %q is incomplete (no pre/post range)", key)
	}

	r := newEngine(layout).RepoFor(workdir, e.SessionID)
	label := journal.DefaultKeyer.Key(e)
	if *stat {
		return diffStatRange(r, label, e.PreSHA, e.PostSHA, args[1:], *jsonOut, stdout, stderr)
	}
	return emitPatchRange(r, label, e.Overlapped, e.PreSHA, e.PostSHA, args[1:], *jsonOut, stdout, stderr)
}

// treeStateOf returns the commit standing for an entry's tree state: its post
// snapshot, or its pre snapshot for pre-only/manual entries.
func treeStateOf(e journal.Entry) string {
	if e.PostSHA != "" {
		return e.PostSHA
	}
	return e.PreSHA
}

// diffTwoEntries diffs two entries' tree states within a single session. The
// pre/post objects live in a per-session shadow repo, so a cross-
// session pair has no common repo and is refused rather than silently guessed.
func diffTwoEntries(layout paths.Layout, workdir string, e1, e2 journal.Entry, paths []string, stat, jsonOut bool, stdout, stderr io.Writer) int {
	if e1.SessionID != e2.SessionID {
		return errf(stderr, "entries belong to different sessions; cross-session diff is not supported")
	}
	if snapshotsReclaimed(layout, workdir, e1) || snapshotsReclaimed(layout, workdir, e2) {
		return errf(stderr, "snapshots were reclaimed by gc; nothing to diff")
	}
	from, to := treeStateOf(e1), treeStateOf(e2)
	if from == "" || to == "" {
		return errf(stderr, "one of the entries has no snapshot to compare")
	}
	overlapped := e1.Overlapped || e2.Overlapped
	if overlapped {
		fmt.Fprintln(stderr, "WARNING: an entry is overlapped — diff attribution may include other concurrent commands")
	}
	label := journal.DefaultKeyer.Key(e1) + ".." + journal.DefaultKeyer.Key(e2)
	r := newEngine(layout).RepoFor(workdir, e1.SessionID)
	if stat {
		return diffStatRange(r, label, from, to, paths, jsonOut, stdout, stderr)
	}
	return emitPatchRange(r, label, overlapped, from, to, paths, jsonOut, stdout, stderr)
}

// emitPatchRange renders the pre..post (or A..B) patch, in text or --json form,
// shared by single-key and cross-entry diff.
func isBadObject(err error) bool {
	var ee *gitx.ExitError
	return errors.As(err, &ee) &&
		(strings.Contains(ee.Stderr, "bad object") || strings.Contains(ee.Stderr, "Not a valid object name"))
}

func emitPatchRange(r *gitx.Repo, label string, overlapped bool, from, to string, paths []string, jsonOut bool, stdout, stderr io.Writer) int {
	patch, err := r.DiffPatch(ctx(), from, to, paths)
	if err != nil {
		if isBadObject(err) {
			return errf(stderr, "snapshots for %q were reclaimed by gc; nothing to diff", label)
		}
		return errf(stderr, "diff: %v", err)
	}
	if jsonOut {
		return emitJSON(stdout, stderr, diffJSON{
			V: outputVersion, Key: label, Overlapped: overlapped, Patch: string(patch), PatchBytes: len(patch),
		})
	}
	if len(patch) == 0 {
		fmt.Fprintln(stdout, "(no content-level changes)")
		return 0
	}
	if len(patch) > patchSoftLimit {
		fmt.Fprintf(stdout, "patch is %d KiB (over the %d KiB display limit); showing the stat summary instead\n",
			len(patch)>>10, patchSoftLimit>>10)
		code := diffStatRange(r, label, from, to, paths, false, stdout, stderr)
		fmt.Fprintln(stdout, patchOverflowHint(label))
		return code
	}
	_, _ = stdout.Write(patch)
	return 0
}

// patchSoftLimit caps the human/agent-facing patch dump. Agents read diff
// output into a bounded context window; past ~2k tokens a stat summary plus an
// explicit escape hatch beats a flood. JSON output is exempt: machine callers
// control their own read size.
const patchSoftLimit = 8 << 10

// patchOverflowHint names a real follow-up command. A cross-entry label is
// "k1..k2", which is a display form, not an argv: rebuild the two-key diff and
// drop the export hint (export takes a single key).
func patchOverflowHint(label string) string {
	if k1, k2, ok := strings.Cut(label, ".."); ok {
		return fmt.Sprintf("full patch: narrow with `bashback diff %s %s <path>...`", k1, k2)
	}
	return fmt.Sprintf("full patch: narrow with `bashback diff %s <path>...`, or `bashback export %s --out <file>`", label, label)
}

// diffJSON is the machine-readable patch payload.
type diffJSON struct {
	V          int    `json:"v"`
	Key        string `json:"key"`
	Overlapped bool   `json:"overlapped"`
	Patch      string `json:"patch"`
	PatchBytes int    `json:"patch_bytes"`
}

type diffStatFileJSON struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

type diffStatJSON struct {
	V       int                `json:"v"`
	Key     string             `json:"key"`
	Files   []diffStatFileJSON `json:"files"`
	Added   int                `json:"added"`
	Deleted int                `json:"deleted"`
}

// diffStatRange merges name-status and numstat into a per-file summary that
// agrees with the patch's numbers, over an arbitrary from..to range —
// shared by single-key and cross-entry diff.
func diffStatRange(r *gitx.Repo, label, from, to string, paths []string, jsonOut bool, stdout, stderr io.Writer) int {
	names, err := r.DiffNameStatus(ctx(), from, to, paths)
	if err != nil {
		if isBadObject(err) {
			return errf(stderr, "snapshots for %q were reclaimed by gc; nothing to diff", label)
		}
		return errf(stderr, "diff --stat: %v", err)
	}
	nums, err := r.DiffNumStat(ctx(), from, to, paths)
	if err != nil {
		if isBadObject(err) {
			return errf(stderr, "snapshots for %q were reclaimed by gc; nothing to diff", label)
		}
		return errf(stderr, "diff --stat: %v", err)
	}
	statusByPath := map[string]string{}
	for _, n := range names {
		statusByPath[n.Path] = n.Status
	}

	files := make([]diffStatFileJSON, 0, len(nums))
	totalAdd, totalDel := 0, 0
	for _, n := range nums {
		binary := n.Added < 0 || n.Deleted < 0
		add, del := n.Added, n.Deleted
		if binary {
			add, del = 0, 0
		}
		totalAdd += add
		totalDel += del
		files = append(files, diffStatFileJSON{
			Path: n.Path, Status: statusByPath[n.Path], Added: add, Deleted: del, Binary: binary,
		})
	}

	if jsonOut {
		return emitJSON(stdout, stderr, diffStatJSON{
			V: outputVersion, Key: label,
			Files: files, Added: totalAdd, Deleted: totalDel,
		})
	}
	if len(files) == 0 {
		fmt.Fprintln(stdout, "(no content-level changes)")
		return 0
	}
	for _, fc := range files {
		if fc.Binary {
			fmt.Fprintf(stdout, " %s  %-40s  (binary)\n", statusOrSpace(fc.Status), fc.Path)
			continue
		}
		fmt.Fprintf(stdout, " %s  %-40s  +%d  -%d\n", statusOrSpace(fc.Status), fc.Path, fc.Added, fc.Deleted)
	}
	fmt.Fprintf(stdout, " %d file%s changed, %d insertion%s(+), %d deletion%s(-)\n",
		len(files), plural(len(files), "", "s"), totalAdd, plural(totalAdd, "", "s"), totalDel, plural(totalDel, "", "s"))
	return 0
}

func statusOrSpace(s string) string {
	if s == "" {
		return " "
	}
	return s
}
