package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/daemon"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/journal"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// GC reclaims expired/over-cap session repos for the current project. Active
// sessions (live socket) are preserved.
func GC(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	cfg := config.Load(layout, workdir, config.OSEnv())
	olderThan := fs.Duration("older-than", cfg.Retention(), "reclaim sessions idle longer than this")
	dryRun := fs.Bool("dry-run", false, "report what would be reclaimed without deleting")
	all := fs.Bool("all", false, "reclaim across every project, not just the current one")
	jsonOut := fs.Bool("json", false, "emit the result as a single JSON object")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bashback gc [--older-than <dur>] [--dry-run] [--all] [--json]")
		fs.PrintDefaults()
	}
	if code, done := parseFS(fs, args, stdout, stderr); done {
		return code
	}

	if *all {
		return gcAll(layout, fs, *olderThan, *dryRun, *jsonOut, stdout, stderr)
	}

	rep, err := newEngine(layout).GC(workdir, snapshot.GCOpts{
		OlderThan:      *olderThan,
		SoftCapBytes:   cfg.SoftCapBytes,
		DryRun:         *dryRun,
		ActiveSessions: liveSessions(layout),
	})
	if err != nil {
		return errf(stderr, "gc: %v", err)
	}
	if *jsonOut {
		return emitJSON(stdout, stderr, gcJSON{
			V: outputVersion, DryRun: rep.DryRun, Removed: rep.Removed,
			FreedBytes: rep.FreedBytes, Kept: len(rep.Kept),
		})
	}
	verb := "reclaimed"
	if rep.DryRun {
		verb = "would reclaim"
	}
	fmt.Fprintf(stdout, "%s %d session(s), freeing %s; kept %d\n", verb, len(rep.Removed), humanBytes(rep.FreedBytes), len(rep.Kept))
	for _, id := range rep.Removed {
		fmt.Fprintf(stdout, "  - %s\n", id)
	}
	return 0
}

type gcJSON struct {
	V          int      `json:"v"`
	DryRun     bool     `json:"dry_run"`
	Removed    []string `json:"removed"`
	FreedBytes int64    `json:"freed_bytes"`
	Kept       int      `json:"kept"`
}

type gcProjectJSON struct {
	Project    string   `json:"project"`
	Removed    []string `json:"removed"`
	FreedBytes int64    `json:"freed_bytes"`
	Kept       int      `json:"kept"`
}

type gcAllJSON struct {
	V          int             `json:"v"`
	DryRun     bool            `json:"dry_run"`
	FreedBytes int64           `json:"freed_bytes"`
	Projects   []gcProjectJSON `json:"projects"`
}

// gcAll sweeps every project using env/default retention uniformly; an explicit
// --older-than still overrides for all projects.
func gcAll(layout paths.Layout, fs *flag.FlagSet, olderThan time.Duration, dryRun, jsonOut bool, stdout, stderr io.Writer) int {
	base := config.Resolve(paths.Meta{}, config.OSEnv())
	effOlder := base.Retention()
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "older-than" {
			effOlder = olderThan
		}
	})

	reports, err := newEngine(layout).GCAll(snapshot.GCOpts{
		OlderThan:      effOlder,
		SoftCapBytes:   base.SoftCapBytes,
		DryRun:         dryRun,
		ActiveSessions: liveSessions(layout),
	})
	if err != nil {
		return errf(stderr, "gc --all: %v", err)
	}

	var totalFreed int64
	totalRemoved := 0
	for _, p := range reports {
		totalFreed += p.Report.FreedBytes
		totalRemoved += len(p.Report.Removed)
	}

	if jsonOut {
		out := gcAllJSON{V: outputVersion, DryRun: dryRun, FreedBytes: totalFreed, Projects: make([]gcProjectJSON, 0, len(reports))}
		for _, p := range reports {
			removed := p.Report.Removed
			if removed == nil {
				removed = []string{}
			}
			out.Projects = append(out.Projects, gcProjectJSON{
				Project: p.Project, Removed: removed, FreedBytes: p.Report.FreedBytes, Kept: len(p.Report.Kept),
			})
		}
		return emitJSON(stdout, stderr, out)
	}

	verb := "reclaimed"
	if dryRun {
		verb = "would reclaim"
	}
	fmt.Fprintf(stdout, "%s %d session(s) across %d project(s), freeing %s\n", verb, totalRemoved, len(reports), humanBytes(totalFreed))
	for _, p := range reports {
		fmt.Fprintf(stdout, "  %s: %d removed, %s, kept %d\n", p.Project, len(p.Report.Removed), humanBytes(p.Report.FreedBytes), len(p.Report.Kept))
		for _, id := range p.Report.Removed {
			fmt.Fprintf(stdout, "    - %s\n", id)
		}
	}
	return 0
}

type doctorCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type doctorJSON struct {
	V      int           `json:"v"`
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
	// Additive; schema v stays 1.
	Wiring   []wiringStatus  `json:"wiring"`
	Activity doctorActivity  `json:"activity"`
	Skill    doctorSkillJSON `json:"skill"`
}

type doctorSkillJSON struct {
	Status string `json:"status"` // ok | stale | missing
	Path   string `json:"path,omitempty"`
}

type doctorActivity struct {
	LastSnapshotTS string `json:"last_snapshot_ts,omitempty"`
	HookErrors24h  int    `json:"hook_errors_24h"`
	LastError      string `json:"last_error,omitempty"`
}

// Doctor runs environment self-checks.
func Doctor(layout paths.Layout, workdir string, args []string, stdout, stderr io.Writer) int {
	repair, args := popFlag(args, "--repair")
	if repair {
		moved, err := journal.Repair(layout.JournalPath(workdir))
		if err != nil {
			return errf(stderr, "journal repair: %v", err)
		}
		if moved == 0 {
			fmt.Fprintln(stdout, "journal clean; nothing to repair")
		} else {
			fmt.Fprintf(stdout, "repaired journal: moved %d bad line(s) to journal.bad\n", moved)
		}
		return 0
	}
	jsonOut, _ := popJSONFlag(args)
	ok := true
	var checks []doctorCheck
	line := func(good bool, format string, a ...any) {
		if !good {
			ok = false
		}
		checks = append(checks, doctorCheck{OK: good, Detail: fmt.Sprintf(format, a...)})
	}

	if v, err := gitx.DetectVersion(ctx(), gitx.ExecRunner{}); err != nil {
		line(false, "git: not detected (%v)", err)
	} else {
		line(v.MeetsMinimum(), "git %s (require >= %d.%d)", v.Raw, gitx.MinMajor, gitx.MinMinor)
	}

	if fi, err := os.Stat(layout.Root); err == nil {
		line(fi.Mode().Perm() == 0o700, "%s permissions %o (want 700)", layout.Root, fi.Mode().Perm())
	} else {
		line(true, "%s not yet created", layout.Root)
	}

	live := liveSessions(layout)
	line(true, "daemon: %d live session(s)", len(live))

	reposRoot := filepath.Join(layout.Root, "repos")
	line(true, "shadow repos disk usage: %s", humanBytes(repoSize(reposRoot)))

	cfg := config.Load(layout, workdir, config.OSEnv())
	line(true, "config max_file_bytes: %s (%s)", humanBytes(cfg.MaxFileBytes), cfg.Sources["max_file_bytes"])
	line(true, "config retention_days: %d (%s)", cfg.RetentionDays, cfg.Sources["retention_days"])
	line(true, "config soft_cap_bytes: %s (%s)", humanBytes(cfg.SoftCapBytes), cfg.Sources["soft_cap_bytes"])
	line(true, "config stale_ttl: %s (%s)", cfg.StaleTTL, cfg.Sources["stale_ttl"])
	line(true, "config idle_timeout: %s (%s)", cfg.IdleTimeout, cfg.Sources["idle_timeout"])
	line(true, "config context_feedback: %s (%s)", cfg.ContextFeedback, cfg.Sources["context_feedback"])
	if len(cfg.ProtectPaths) > 0 {
		line(true, "config protect_paths: %s (%s)", strings.Join(cfg.ProtectPaths, ", "), cfg.Sources["protect_paths"])
	}
	if len(cfg.ForceInclude) > 0 {
		line(true, "force_include for this project: %s (%s)", strings.Join(cfg.ForceInclude, ", "), cfg.Sources["force_include"])
	} else {
		line(true, "force_include: none")
	}

	// Shadow repos created before the perf config keep working but miss the
	// speedups; new repos get it at Init. Informational only — no silent migration.
	if entries, err := readView(layout, workdir); err == nil && len(entries) > 0 {
		sid := entries[len(entries)-1].SessionID
		r := newEngine(layout).RepoFor(workdir, sid)
		if r.Initialized() && r.ConfigGet(ctx(), "index.version") != "4" {
			line(true, "config: latest session repo predates git perf config (index.version=4); new sessions are configured automatically")
		}
	}

	// Wiring: are the hooks actually wired into a Claude settings.json?
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	claudeWiring := checkWiring(workdir, home, exe)
	codexWiring := checkCodexWiring(workdir, home, exe)
	cursorWiring := checkCursorWiring(workdir, home, exe)
	otherWired := anyWired(codexWiring) || anyWired(cursorWiring)
	claudeFound := 0
	claudeUnreadable := false
	for _, w := range claudeWiring {
		switch w.Status {
		case "ok", "stale path":
			claudeFound++
		case "unreadable":
			claudeUnreadable = true
		}
	}
	if claudeFound == 0 && !claudeUnreadable && claudePluginInstalled(home) {
		line(true, "claude: wired via plugin")
		for i := range claudeWiring {
			claudeWiring[i].Status = "wired via plugin"
		}
	} else if claudeFound == 0 && !claudeUnreadable && otherWired {
		line(true, "claude: not wired (another platform is wired)")
	} else {
		wiringBroken := false
		for _, w := range claudeWiring {
			switch w.Status {
			case "ok":
				detail := "wiring " + wiringLabel(w) + ": ok"
				if w.Drift {
					detail += " (hooks point at a different binary)"
				}
				line(true, "%s", detail)
			case "unreadable":
				line(false, "wiring: settings file unreadable: %s", w.File)
				wiringBroken = true
			default: // missing | stale path
				line(false, "wiring %s: %s", wiringLabel(w), w.Status)
				wiringBroken = true
			}
		}
		if wiringBroken {
			line(false, "run 'bashback install' to wire hooks")
		}
	}

	emitPlatformWiring := func(rows []wiringStatus) {
		for _, w := range rows {
			switch w.Status {
			case "not wired":
				line(true, "%s: not wired", w.Platform)
			case "ok":
				detail := fmt.Sprintf("%s wiring %s: ok", w.Platform, wiringLabel(w))
				if w.Drift {
					detail += " (hooks point at a different binary)"
				}
				line(true, "%s", detail)
			case "unreadable":
				line(false, "%s: settings file unreadable: %s", w.Platform, w.File)
			default: // missing | stale path
				line(false, "%s wiring %s: %s", w.Platform, wiringLabel(w), w.Status)
			}
		}
	}
	emitPlatformWiring(codexWiring)
	emitPlatformWiring(cursorWiring)

	// Skill: informational only — a missing skill weakens agent education but
	// never breaks snapshot protection.
	skillSt, skillPath := skillStatus(workdir, home)
	switch skillSt {
	case "ok":
		line(true, "skill: ok (%s)", skillPath)
	case "stale":
		line(true, "skill: stale (%s); re-run 'bashback install' to refresh", skillPath)
	default:
		line(true, "skill: not installed; 'bashback install' writes it next to settings.json")
	}

	// Activity: is protection actually running?
	lastTS := lastSnapshotTS(layout.JournalPath(workdir))
	if lastTS != "" {
		line(true, "last snapshot: %s", humanAge(lastTS, time.Now()))
	} else {
		line(true, "last snapshot: never")
	}
	errs24h, lastErr := hookLogSummary(layout.HookLogPath(workdir), time.Now())
	if errs24h == 0 && lastErr == "" {
		line(true, "no recorded hook errors")
	} else {
		line(true, "%d hook error(s) in the last 24h", errs24h)
		if lastErr != "" {
			line(true, "last hook error: %s", lastErr)
		}
	}

	for _, sess := range staleIndexLocks(layout, workdir, time.Now()) {
		line(true, "stale index.lock under %s; next snapshot will recover it", sess)
	}

	for _, pat := range excludedFromProtection(layout, workdir) {
		line(true, "excluded from protection: %s (info/exclude)", pat)
	}

	if jsonOut {
		allWiring := append(append(claudeWiring, codexWiring...), cursorWiring...)
		emitJSON(stdout, stderr, doctorJSON{
			V: outputVersion, OK: ok, Checks: checks,
			Wiring:   allWiring,
			Activity: doctorActivity{LastSnapshotTS: lastTS, HookErrors24h: errs24h, LastError: lastErr},
			Skill:    doctorSkillJSON{Status: skillSt, Path: skillPath},
		})
		if !ok {
			return 1
		}
		return 0
	}
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(stdout, "[%s] %s\n", mark, c.Detail)
	}
	if !ok {
		return 1
	}
	return 0
}

// excludedFromProtection collects the non-default info/exclude patterns across
// the project's session repos: runtime exclusions (oversized/unreadable files,
// protect_paths) that a snapshot will not capture. The forced `.git/` entry,
// sparse-checkout markers, comments and blanks are skipped; the rest is surfaced
// deduped so the user sees what protection silently drops.
func excludedFromProtection(layout paths.Layout, workdir string) []string {
	sessions := filepath.Join(layout.RepoDir(workdir), "sessions")
	ents, err := os.ReadDir(sessions)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(sessions, e.Name(), "info", "exclude"))
		if rerr != nil {
			continue
		}
		for _, raw := range strings.Split(string(b), "\n") {
			p := strings.TrimSpace(raw)
			if p == "" || strings.HasPrefix(p, "#") || p == ".git/" || strings.HasPrefix(p, "/.git") {
				continue
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// staleIndexLocks lists session repos whose index.lock is older than the
// recovery threshold; the next snapshot for that session clears it, so this is
// informational, not a failure.
func staleIndexLocks(layout paths.Layout, workdir string, now time.Time) []string {
	sessions := filepath.Join(layout.RepoDir(workdir), "sessions")
	ents, err := os.ReadDir(sessions)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		fi, serr := os.Stat(filepath.Join(sessions, e.Name(), "index.lock"))
		if serr != nil {
			continue
		}
		if now.Sub(fi.ModTime()) > 5*time.Minute {
			out = append(out, e.Name())
		}
	}
	return out
}

// anyWired reports whether a platform's wiring rows show it is actually wired
// (at least one "ok" row), as opposed to absent ("not wired"/missing).
func anyWired(rows []wiringStatus) bool {
	for _, w := range rows {
		if w.Status == "ok" {
			return true
		}
	}
	return false
}

// wiringLabel renders a wiring row's hook event with its matcher, when present.
func wiringLabel(w wiringStatus) string {
	if w.Matcher == "" {
		return w.Event
	}
	return w.Event + "[" + w.Matcher + "]"
}

// lastSnapshotTS returns the most recent entry timestamp in the journal, or ""
// if the journal is absent/empty. TS is RFC3339 UTC across all writers, so a
// lexical max equals the chronological max; note-only half-rows carry no TS and
// never win.
func lastSnapshotTS(jpath string) string {
	entries, err := journal.ReadMerged(jpath, journal.DefaultKeyer)
	if err != nil {
		return ""
	}
	max := ""
	for _, e := range entries {
		if e.TS > max {
			max = e.TS
		}
	}
	return max
}

// hookLogSummary parses hook.log and returns the count of errors
// within the last 24h plus a one-line summary of the most recent entry. A
// missing/unreadable line is skipped; a missing file yields (0, "").
func hookLogSummary(path string, now time.Time) (count24h int, lastSummary string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = f.Close() }()
	cutoff := now.Add(-24 * time.Hour)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" {
			continue
		}
		var rec struct {
			TS   string `json:"ts"`
			Hook string `json:"hook"`
			Err  string `json:"err"`
		}
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			continue
		}
		if t, perr := time.Parse(time.RFC3339, rec.TS); perr == nil && t.After(cutoff) {
			count24h++
		}
		lastSummary = fmt.Sprintf("%s %s: %s", rec.TS, rec.Hook, rec.Err)
	}
	return count24h, lastSummary
}

// liveSessions maps session ids with a live socket to true.
func liveSessions(layout paths.Layout) map[string]bool {
	active := map[string]bool{}
	ents, err := os.ReadDir(layout.RunDir())
	if err != nil {
		return active
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		if daemon.SocketAlive(filepath.Join(layout.RunDir(), e.Name())) {
			active[strings.TrimSuffix(e.Name(), ".sock")] = true
		}
	}
	return active
}
