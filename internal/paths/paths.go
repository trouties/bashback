// Package paths owns the on-disk layout under ~/.bashback and the workdir hash
// that namespaces a project's shadow repos. Everything it creates is 0700: the
// shadow repos hold verbatim copies of project files.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaVersion is the meta.json format version this build writes and is the
// highest it will read; a higher on-disk version is a hard read error.
const SchemaVersion = 1

// dirPerm is applied to every directory bashback creates. The shadow repos can
// contain force-included secrets, so the tree is owner-only.
const dirPerm = 0o700

// Layout resolves all bashback paths beneath a single root (~/.bashback).
type Layout struct {
	Root string
}

// New returns a Layout rooted at an explicit directory (used by tests).
func New(root string) Layout { return Layout{Root: root} }

// Default resolves the layout from $BASHBACK_HOME, falling back to ~/.bashback.
func Default() (Layout, error) {
	if h := os.Getenv("BASHBACK_HOME"); h != "" {
		return Layout{Root: h}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolve home dir: %w", err)
	}
	return Layout{Root: filepath.Join(home, ".bashback")}, nil
}

// WorkdirHash maps a project's absolute path to the 16-hex-char namespace used
// under repos/. The path is cleaned first so "/p" and "/p/" share one repo.
func WorkdirHash(workdir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(workdir)))
	return hex.EncodeToString(sum[:])[:16]
}

func (l Layout) RepoDir(workdir string) string {
	return filepath.Join(l.Root, "repos", WorkdirHash(workdir))
}

// safeSessionID confines a session id to filename-safe characters. Hook stdin
// is structurally untrusted: an id with a path separator or any character
// outside the whitelist is replaced by a hash-derived name, never used as-is.
func safeSessionID(id string) string {
	if id == "" {
		return "h" + shortHash("")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return "h" + shortHash(id)
		}
	}
	return id
}

func (l Layout) SessionGitDir(workdir, sessionID string) string {
	return filepath.Join(l.RepoDir(workdir), "sessions", safeSessionID(sessionID)+".git")
}

func (l Layout) JournalPath(workdir string) string {
	return filepath.Join(l.RepoDir(workdir), "journal.jsonl")
}

// HookLogPath is the per-project best-effort error log the hook writes its
// swallowed failures to. It sits beside the journal so doctor can
// summarize it from the same project dir.
func (l Layout) HookLogPath(workdir string) string {
	return filepath.Join(l.RepoDir(workdir), "hook.log")
}

func (l Layout) MetaPath(workdir string) string {
	return filepath.Join(l.RepoDir(workdir), "meta.json")
}

// sunPathBudget is a conservative ceiling for the full socket path: the
// sockaddr_un.sun_path field caps at 104 bytes on Darwin (108 on Linux), and
// both bind and connect enforce it. Kept under 104 with margin.
const sunPathBudget = 100

// RunDir holds the per-session sockets. It is normally <Root>/run, but a deep
// Root (a long $TMPDIR in tests, or an unusual BASHBACK_HOME) plus a UUID
// session id would overflow sun_path; in that case sockets relocate to a short,
// root-namespaced dir so bind/connect stay legal. Production (~/.bashback) is
// short and never relocates.
func (l Layout) RunDir() string {
	run := filepath.Join(l.Root, "run")
	// Longest session id we emit is a 36-char UUID; + separator + ".sock".
	if len(run)+1+36+len(".sock") > sunPathBudget {
		return filepath.Join(shortTmpBase(), "bashback-"+shortHash(l.Root))
	}
	return run
}

// SocketPath lives under RunDir to keep the sockaddr_un path below the limit.
func (l Layout) SocketPath(sessionID string) string {
	return filepath.Join(l.RunDir(), safeSessionID(sessionID)+".sock")
}

// shortTmpBase prefers /tmp (a short symlink on macOS) so the relocated socket
// dir stays within the sun_path budget; os.TempDir() ($TMPDIR) is itself long
// on macOS and can't be the fallback base.
func shortTmpBase() string {
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		return "/tmp"
	}
	return os.TempDir()
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func (l Layout) LogPath() string { return filepath.Join(l.Root, "log", "bashback.log") }

// EnsureRepoDirs creates the root, run, log, and per-project repo/sessions dirs
// (all 0700) so callers can write into them without further mkdir.
func (l Layout) EnsureRepoDirs(workdir string) error {
	dirs := []string{
		l.Root,
		l.RunDir(),
		filepath.Dir(l.LogPath()),
		l.RepoDir(workdir),
		filepath.Join(l.RepoDir(workdir), "sessions"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		// MkdirAll honors umask, which can strip bits off 0700; force it back.
		if err := os.Chmod(d, dirPerm); err != nil {
			return fmt.Errorf("chmod %s: %w", d, err)
		}
	}
	return nil
}

// Meta is the per-project meta.json: format version, the original (pre-hash)
// path for audit, creation time, and the force-include list.
type Meta struct {
	SchemaVersion int      `json:"schema_version"`
	OriginalPath  string   `json:"original_path"`
	CreatedAt     string   `json:"created_at"`
	ForceInclude  []string `json:"force_include,omitempty"`

	// Additive per-project config keys; schema_version stays 1.
	MaxFileBytes    int64    `json:"max_file_bytes,omitempty"`
	RetentionDays   int      `json:"retention_days,omitempty"`
	SoftCapBytes    int64    `json:"soft_cap_bytes,omitempty"`
	ContextFeedback string   `json:"context_feedback,omitempty"`
	ProtectPaths    []string `json:"protect_paths,omitempty"`
}

func (l Layout) WriteMeta(workdir string, m Meta) error {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// tmp+rename so a concurrent reader never sees a half-written meta.json. The
	// tmp lives in the same dir as the target to keep the rename on one volume.
	tmp := l.MetaPath(workdir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.MetaPath(workdir))
}

// ReadMeta loads meta.json. A schema_version newer than this build is a hard
// error rather than a guess (format versioning).
func (l Layout) ReadMeta(workdir string) (Meta, error) {
	b, err := os.ReadFile(l.MetaPath(workdir))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("parse meta.json: %w", err)
	}
	if m.SchemaVersion > SchemaVersion {
		return Meta{}, fmt.Errorf("meta.json schema_version %d is newer than supported %d", m.SchemaVersion, SchemaVersion)
	}
	return m, nil
}
