package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkdirHash(t *testing.T) {
	h := WorkdirHash("/home/u/project")
	if len(h) != 16 {
		t.Fatalf("hash len = %d, want 16 (got %q)", len(h), h)
	}
	for _, r := range h {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("hash %q has non-hex rune %q", h, r)
		}
	}
	if WorkdirHash("/home/u/project") != h {
		t.Fatal("hash not deterministic")
	}
	if WorkdirHash("/home/u/other") == h {
		t.Fatal("distinct paths collided")
	}
	// trailing slash / uncleaned path must normalize to the same repo.
	if WorkdirHash("/home/u/project/") != h {
		t.Fatal("trailing slash changed hash; path not cleaned")
	}
}

func TestLayoutPaths(t *testing.T) {
	l := New("/root/.bashback")
	wd := "/home/u/project"
	hash := WorkdirHash(wd)

	if got, want := l.RepoDir(wd), filepath.Join("/root/.bashback/repos", hash); got != want {
		t.Errorf("RepoDir = %q, want %q", got, want)
	}
	if got, want := l.SessionGitDir(wd, "sid"), filepath.Join("/root/.bashback/repos", hash, "sessions", "sid.git"); got != want {
		t.Errorf("SessionGitDir = %q, want %q", got, want)
	}
	if got, want := l.JournalPath(wd), filepath.Join("/root/.bashback/repos", hash, "journal.jsonl"); got != want {
		t.Errorf("JournalPath = %q, want %q", got, want)
	}
	if got, want := l.SocketPath("sid"), filepath.Join("/root/.bashback/run", "sid.sock"); got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
	if got, want := l.LogPath(), filepath.Join("/root/.bashback/log", "bashback.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

func TestSocketPathStaysUnderUnixLimit(t *testing.T) {
	// run/ is deliberately a short path to dodge the 104-byte sockaddr_un limit.
	l := New("/home/averagelynameduser/.bashback")
	p := l.SocketPath("a1b2c3d4-e5f6-7890-abcd-ef0123456789") // UUID-shaped session id
	if len(p) >= 104 {
		t.Fatalf("socket path %d bytes, must stay < 104: %q", len(p), p)
	}
}

// A deep root (long $TMPDIR on macOS, or odd BASHBACK_HOME) would push the
// socket past the sun_path limit; RunDir must relocate it to a short dir so the
// full socket path stays legal even with a UUID session id.
func TestSocketPathRelocatesForDeepRoot(t *testing.T) {
	deep := "/var/folders/8d/778wjbv96mq1760tv6gk374m0000gn/T/TestSomethingVeryLongIndeed3656691085/001/.bashback"
	l := New(deep)
	p := l.SocketPath("a1b2c3d4-e5f6-7890-abcd-ef0123456789")
	if len(p) >= 104 {
		t.Fatalf("relocated socket path still %d bytes: %q", len(p), p)
	}
	if strings.HasPrefix(l.RunDir(), deep) {
		t.Fatalf("deep root should relocate RunDir off %q, got %q", deep, l.RunDir())
	}
	// The same root must resolve to the same socket dir (deterministic, so a
	// client and the daemon agree).
	if New(deep).RunDir() != l.RunDir() {
		t.Fatal("RunDir must be deterministic for a given root")
	}
}

func TestEnsureRepoDirsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perm bits")
	}
	root := t.TempDir()
	l := New(filepath.Join(root, ".bashback"))
	wd := "/home/u/project"
	if err := l.EnsureRepoDirs(wd); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{l.Root, l.RepoDir(wd), filepath.Join(l.RepoDir(wd), "sessions"), l.RunDir(), filepath.Dir(l.LogPath())} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s perm = %o, want 700", p, perm)
		}
	}
}

func TestMetaRoundTrip(t *testing.T) {
	root := t.TempDir()
	l := New(filepath.Join(root, ".bashback"))
	wd := "/home/u/project"
	if err := l.EnsureRepoDirs(wd); err != nil {
		t.Fatal(err)
	}
	m := Meta{SchemaVersion: SchemaVersion, OriginalPath: wd, CreatedAt: "2026-06-10T00:00:00Z", ForceInclude: []string{".env"}}
	if err := l.WriteMeta(wd, m); err != nil {
		t.Fatal(err)
	}
	got, err := l.ReadMeta(wd)
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginalPath != wd || len(got.ForceInclude) != 1 || got.ForceInclude[0] != ".env" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestReadMetaRejectsFutureVersion(t *testing.T) {
	root := t.TempDir()
	l := New(filepath.Join(root, ".bashback"))
	wd := "/home/u/project"
	if err := l.EnsureRepoDirs(wd); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.RepoDir(wd), "meta.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":999,"original_path":"/x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadMeta(wd); err == nil {
		t.Fatal("reading a future schema_version must error, not guess")
	}
}

func TestReadMetaMissingIsNotExist(t *testing.T) {
	root := t.TempDir()
	l := New(filepath.Join(root, ".bashback"))
	if _, err := l.ReadMeta("/home/u/project"); !os.IsNotExist(err) {
		t.Fatalf("missing meta should be IsNotExist, got %v", err)
	}
}

func TestHookLogPath(t *testing.T) {
	l := New("/r/.bashback")
	wd := "/home/u/project"
	got := l.HookLogPath(wd)
	if dir := filepath.Dir(got); dir != filepath.Dir(l.JournalPath(wd)) {
		t.Fatalf("hook.log dir %q != journal dir %q", dir, filepath.Dir(l.JournalPath(wd)))
	}
	if base := filepath.Base(got); base != "hook.log" {
		t.Fatalf("hook.log base = %q, want hook.log", base)
	}
}

func TestSessionPathsRejectTraversal(t *testing.T) {
	l := New(t.TempDir())
	for _, sid := range []string{"../../../../tmp/evil", "a/b", "x/../../y", ""} {
		git := l.SessionGitDir("/proj", sid)
		if !strings.HasPrefix(git, l.RepoDir("/proj")+string(os.PathSeparator)) {
			t.Errorf("SessionGitDir(%q) = %q escapes the repo dir", sid, git)
		}
		sock := l.SocketPath(sid)
		if !strings.HasPrefix(sock, l.RunDir()+string(os.PathSeparator)) {
			t.Errorf("SocketPath(%q) = %q escapes the run dir", sid, sock)
		}
	}
}

func TestSessionPathsStableForValidIDs(t *testing.T) {
	l := New(t.TempDir())
	want := filepath.Join(l.RepoDir("/proj"), "sessions", "abc-123_X.9.git")
	if got := l.SessionGitDir("/proj", "abc-123_X.9"); got != want {
		t.Errorf("valid id rewritten: %q != %q", got, want)
	}
}

func TestWriteMetaIsAtomic(t *testing.T) {
	root := t.TempDir()
	l := New(filepath.Join(root, ".bashback"))
	wd := "/home/u/project"
	if err := l.EnsureRepoDirs(wd); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteMeta(wd, Meta{OriginalPath: wd}); err != nil {
		t.Fatal(err)
	}
	// No tmp file is left behind, and the result is a valid, readable meta.json.
	if _, err := os.Stat(l.MetaPath(wd) + ".tmp"); !os.IsNotExist(err) {
		t.Error("WriteMeta left a .tmp file behind")
	}
	if _, err := l.ReadMeta(wd); err != nil {
		t.Fatalf("meta.json invalid after atomic write: %v", err)
	}
}
