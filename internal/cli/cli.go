// Package cli implements the user-facing subcommands (list/diff/restore/gc/
// doctor). Each is a function returning an exit code and taking explicit
// writers and a workdir, so it is testable without a process.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/lockfile"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// acquireProjectLock serializes work-tree writers (the restore family and manual
// snap) against the degraded hook path, which takes the same per-project lock.
// Snapshot reads stay lock-free. On contention it returns a nil lock and the exit
// code the caller should propagate.
func acquireProjectLock(layout paths.Layout, workdir string, stderr io.Writer) (*lockfile.Lock, int) {
	if err := layout.EnsureRepoDirs(workdir); err != nil {
		return nil, errf(stderr, "%v", err)
	}
	l, err := lockfile.Acquire(filepath.Join(layout.RepoDir(workdir), "lock"), 2*time.Second)
	if err != nil {
		return nil, errf(stderr, "another bashback process is writing snapshots for this project; retry in a moment")
	}
	return l, 0
}

func newEngine(layout paths.Layout) *snapshot.Engine {
	eng := snapshot.New(layout, gitx.ExecRunner{})
	eng.MaxFileBytesFor = func(workdir string) int64 {
		return config.Load(layout, workdir, config.OSEnv()).MaxFileBytes
	}
	eng.ProtectPathsFor = func(workdir string) []string {
		return config.Load(layout, workdir, config.OSEnv()).ProtectPaths
	}
	return eng
}

// tooShortError marks a ref refused only for being a too-short key prefix
// (<4 chars). Callers whose argument has a non-key fallback (diff's second
// positional, which may be a path filter) treat it like a miss; everywhere
// else it surfaces verbatim.
type tooShortError struct{ ref string }

func (e tooShortError) Error() string {
	return fmt.Sprintf("key prefix %q is too short (use at least 4 characters, @N, or the full key)", e.ref)
}

// resolveEntry locates a merged journal entry by reference: an `@N` relative
// index (ts-descending, `@1` newest), an exact key, or an unambiguous key prefix.
// `@` never collides with a key (no key starts with `@`). Overlap is computed over
// the whole merged view first, so the returned entry carries its read-time flag.
// Returns (entry, found, err): a structural error (bad/out-of-range `@N`, ambiguous
// prefix) is err; a plain miss is found=false, err=nil.
func resolveEntry(layout paths.Layout, workdir, ref string) (journal.Entry, bool, error) {
	entries, err := readView(layout, workdir)
	if err != nil {
		return journal.Entry{}, false, err
	}
	return resolveEntryIn(entries, ref)
}

// resolveEntryIn is resolveEntry over an already-read merged view, so callers
// that hold the entries (rewind's span, diff's two-key form) do not re-read and
// re-mark the journal. See resolveEntry for the addressing semantics.
func resolveEntryIn(entries []journal.Entry, ref string) (journal.Entry, bool, error) {
	if strings.HasPrefix(ref, "@") {
		return resolveAtN(entries, ref)
	}
	for _, e := range entries {
		if journal.DefaultKeyer.Key(e) == ref || e.ToolUseID == ref {
			return e, true, nil
		}
	}
	// A too-short fragment is refused before the prefix scan: a 1–3 char ref could
	// uniquely match today and silently resolve to a different entry tomorrow, so we
	// require a deliberate prefix. Exact keys and @N are handled above.
	if len(ref) < 4 {
		return journal.Entry{}, false, tooShortError{ref: ref}
	}
	// Unambiguous key-prefix fallback so the long opaque keys are usable.
	var match journal.Entry
	n := 0
	for _, e := range entries {
		k := journal.DefaultKeyer.Key(e)
		if k != "" && strings.HasPrefix(k, ref) {
			match, n = e, n+1
		}
	}
	if n == 1 {
		return match, true, nil
	}
	if n > 1 {
		return journal.Entry{}, false, fmt.Errorf("key prefix %q is ambiguous (%d entries match); use a longer prefix or the full key", ref, n)
	}
	return journal.Entry{}, false, nil
}

// resolveAtN maps `@N` to the Nth-newest merged entry. The ts-descending order
// breaks same-second ties by journal row order with the later-written row
// first (see tsDescOrder), matching `list`.
func resolveAtN(entries []journal.Entry, ref string) (journal.Entry, bool, error) {
	n, err := strconv.Atoi(ref[1:])
	if err != nil || n < 1 {
		return journal.Entry{}, false, fmt.Errorf("invalid reference %q (use @1 for the newest entry)", ref)
	}
	ordered := tsDescOrder(entries)
	if n > len(ordered) {
		return journal.Entry{}, false, fmt.Errorf("@%d is out of range (%d entries)", n, len(ordered))
	}
	return ordered[n-1], true, nil
}

// tsDescOrder returns entries newest-first by ts. ts has only second granularity,
// so same-second rows are tie-broken by journal row order, later-written (later
// event) first — making `@1` newest even within one second. The incoming slice is
// in file order, so the slice index is the row order.
func tsDescOrder(entries []journal.Entry) []journal.Entry {
	type indexed struct {
		e   journal.Entry
		idx int
	}
	tmp := make([]indexed, len(entries))
	for i, e := range entries {
		tmp[i] = indexed{e: e, idx: i}
	}
	sort.SliceStable(tmp, func(i, j int) bool {
		if tmp[i].e.TS != tmp[j].e.TS {
			return tmp[i].e.TS > tmp[j].e.TS
		}
		return tmp[i].idx > tmp[j].idx
	})
	ordered := make([]journal.Entry, len(tmp))
	for i := range tmp {
		ordered[i] = tmp[i].e
	}
	return ordered
}

// atNIndex maps each merged entry's key to its 1-based @N index (ts-descending),
// so list/log can show the relative addressing column.
func atNIndex(entries []journal.Entry) map[string]int {
	ordered := tsDescOrder(entries)
	idx := make(map[string]int, len(ordered))
	for i, e := range ordered {
		idx[journal.DefaultKeyer.Key(e)] = i + 1
	}
	return idx
}

// parseFS runs a FlagSet so an explicit -h lands on stdout with exit 0, a real
// parse error on stderr with exit 2, and a clean parse continues (done=false).
func parseFS(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) (code int, done bool) {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	switch err := fs.Parse(args); {
	case err == nil:
		return 0, false
	case errors.Is(err, flag.ErrHelp):
		_, _ = stdout.Write(buf.Bytes())
		return 0, true
	default:
		_, _ = stderr.Write(buf.Bytes())
		return 2, true
	}
}

// parseFlagsAnywhere parses fs while tolerating flags that appear after positional
// arguments. Go's flag package stops at the first non-flag token, so
// `restore <key> --force` would otherwise silently drop --force. It routes an
// explicit -h to stdout (exit 0) and a parse error to stderr (exit 2) like
// parseFS, returning the collected positional arguments when done=false.
func parseFlagsAnywhere(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) (positional []string, code int, done bool) {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	rest := args
	for {
		switch err := fs.Parse(rest); {
		case err == nil:
			// fall through
		case errors.Is(err, flag.ErrHelp):
			_, _ = stdout.Write(buf.Bytes())
			return nil, 0, true
		default:
			_, _ = stderr.Write(buf.Bytes())
			return nil, 2, true
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return positional, 0, false
}

// nearestProtectedParent walks up from workdir looking for an ancestor whose
// repo dir has a meta.json — the likely intended project root when bashback is
// invoked from a subdirectory (workdir hashing is exact, not inherited).
func nearestProtectedParent(layout paths.Layout, workdir string) string {
	dir := filepath.Dir(filepath.Clean(workdir))
	for range 8 {
		if _, err := os.Stat(layout.MetaPath(dir)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// readView returns the merged journal with cross-session overlap flags applied.
func readView(layout paths.Layout, workdir string) ([]journal.Entry, error) {
	entries, err := journal.ReadMerged(layout.JournalPath(workdir), journal.DefaultKeyer)
	if err != nil {
		return nil, err
	}
	return journal.MarkOverlaps(entries), nil
}

// isPreOnly reports whether a merged entry is an orphan pre: a pre snapshot with
// no post (interrupted command or permission denial).
func isPreOnly(e journal.Entry) bool {
	return e.PreSHA != "" && e.PostSHA == ""
}

// snapshotsReclaimed reports whether an entry's snapshots are gone: the session
// repo was removed by gc, or it exists but no longer holds the entry's commits
// (a failed restore once recreated an empty repo; directory presence alone lies).
func snapshotsReclaimed(layout paths.Layout, workdir string, e journal.Entry) bool {
	dir := layout.SessionGitDir(workdir, e.SessionID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return true
	}
	sha := e.PreSHA
	if sha == "" {
		sha = e.PostSHA
	}
	if sha == "" {
		return false
	}
	r := gitx.NewRepo(dir, workdir, gitx.ExecRunner{})
	return !r.CommitExists(ctx(), sha)
}

// reclaimedMemo caches snapshotsReclaimed per session for one render pass: list
// calls it once per row, but the answer only varies per session GIT_DIR. Scoped
// to a single command invocation — never process-global — so a gc or test that
// removes a repo mid-process cannot observe a stale value.
type reclaimedMemo struct {
	layout  paths.Layout
	workdir string
	seen    map[string]bool
}

func newReclaimedMemo(layout paths.Layout, workdir string) *reclaimedMemo {
	return &reclaimedMemo{layout: layout, workdir: workdir, seen: map[string]bool{}}
}

func (m *reclaimedMemo) reclaimed(e journal.Entry) bool {
	if v, ok := m.seen[e.SessionID]; ok {
		return v
	}
	v := snapshotsReclaimed(m.layout, m.workdir, e)
	m.seen[e.SessionID] = v
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func ctx() context.Context { return context.Background() }

func repoSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func errf(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, "bashback: "+format+"\n", args...)
	return 1
}

// stringSliceFlag is a repeatable string flag (e.g. --status protected --status
// restored).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseSince accepts a Go-style duration extended with a day unit (2h, 3d, 90m)
// or an absolute RFC3339 timestamp, returning the cutoff time. now anchors
// relative durations.
func parseSince(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if strings.HasSuffix(v, "d") {
		var days float64
		if _, err := fmt.Sscanf(v, "%fd", &days); err == nil {
			return now.Add(-time.Duration(days * float64(24*time.Hour))), nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q (use 2h, 3d, or RFC3339)", v)
	}
	return now.Add(-d), nil
}

// humanAge renders an RFC3339 timestamp as a compact relative age ("5m ago").
func humanAge(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
