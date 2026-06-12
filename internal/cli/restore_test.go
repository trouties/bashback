package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withScript overrides interactive-restore injection points for one test: a
// scripted input reader and a forced-TTY predicate, restored on cleanup.
func withScript(t *testing.T, input string) {
	t.Helper()
	oldIn, oldTTY := restoreInput, isInteractive
	restoreInput = strings.NewReader(input)
	isInteractive = func() bool { return true }
	t.Cleanup(func() { restoreInput = oldIn; isInteractive = oldTTY })
}

func readWork(t *testing.T, f *fix, name string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.work, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// A scripted y/n selection reverts only the chosen hunk and shows the hunk prompt.
func TestRestoreInteractiveScriptedSelection(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", thirtyLines(nil))
	f.capture(t, "tool_p", "x", func() {
		f.write(t, "f.txt", thirtyLines(map[int]string{3: "X3", 27: "X27"}))
	})

	withScript(t, "y\nn\n") // revert first hunk, keep second
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_p", "-p"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	v, _ := readWork(t, f, "f.txt")
	if want := thirtyLines(map[int]string{27: "X27"}); v != want {
		t.Fatalf("result =\n%q\nwant\n%q", v, want)
	}
	if !strings.Contains(out.String(), "[y,n,a,d,q,?]") {
		t.Fatalf("missing hunk prompt:\n%s", out.String())
	}
}

// A command-created file gets a delete prompt; a binary change a whole-file revert.
func TestRestoreInteractiveWholeFile(t *testing.T) {
	f := newFix(t)
	pre := []byte{0, 1, 2, 255}
	if err := os.WriteFile(filepath.Join(f.work, "bin.dat"), pre, 0o644); err != nil {
		t.Fatal(err)
	}
	f.capture(t, "tool_w", "x", func() {
		if err := os.WriteFile(filepath.Join(f.work, "bin.dat"), []byte{255, 254, 0, 9}, 0o644); err != nil {
			t.Fatal(err)
		}
		f.write(t, "new.txt", "created")
	})

	// Alphabetical: bin.dat (revert) then new.txt (delete).
	withScript(t, "y\ny\n")
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_w", "-p"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "delete new.txt? [y/n]") {
		t.Fatalf("missing delete prompt:\n%s", out.String())
	}
	if _, ok := readWork(t, f, "new.txt"); ok {
		t.Error("created file should be deleted")
	}
	if b, _ := os.ReadFile(filepath.Join(f.work, "bin.dat")); !bytes.Equal(b, pre) {
		t.Errorf("bin.dat = %v, want pre %v", b, pre)
	}
}

func treeSnap(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, rerr := os.ReadFile(p)
		out[rel] = string(b)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Answering yes to everything leaves the same work-tree as a whole restore.
func TestRestoreInteractiveAllYesEqualsFull(t *testing.T) {
	setup := func(f *fix) {
		f.write(t, "mod.txt", thirtyLines(nil))
		f.write(t, "gone.txt", "doomed")
		f.capture(t, "tool_a", "x", func() {
			f.write(t, "mod.txt", thirtyLines(map[int]string{3: "A", 27: "B"}))
			os.Remove(filepath.Join(f.work, "gone.txt"))
			f.write(t, "added.txt", "new")
		})
	}

	fr := newFix(t)
	setup(fr)
	var o, e bytes.Buffer
	if code := Restore(fr.layout, fr.work, []string{"tool_a"}, &o, &e); code != 0 {
		t.Fatalf("full restore exit %d: %s", code, e.String())
	}

	fp := newFix(t)
	setup(fp)
	withScript(t, strings.Repeat("y\n", 10))
	var o2, e2 bytes.Buffer
	if code := Restore(fp.layout, fp.work, []string{"tool_a", "-p"}, &o2, &e2); code != 0 {
		t.Fatalf("interactive restore exit %d: %s", code, e2.String())
	}

	got, want := treeSnap(t, fp.work), treeSnap(t, fr.work)
	if len(got) != len(want) {
		t.Fatalf("file sets differ: interactive=%v full=%v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("file %q: interactive=%q full=%q", k, got[k], v)
		}
	}
}

// Quitting at the first prompt is a clean no-op: exit 0, no journal row, no work-tree change.
func TestRestoreInteractiveQuitNoSideEffects(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", thirtyLines(nil))
	f.capture(t, "tool_q", "x", func() {
		f.write(t, "f.txt", thirtyLines(map[int]string{3: "X3"}))
	})
	post, _ := readWork(t, f, "f.txt")
	before, _ := readView(f.layout, f.work)

	withScript(t, "q\n")
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_q", "-p"}, &out, &errb); code != 0 {
		t.Fatalf("quit exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nothing selected") {
		t.Fatalf("missing no-op message:\n%s", out.String())
	}
	if v, _ := readWork(t, f, "f.txt"); v != post {
		t.Fatal("quit must not change the work-tree")
	}
	after, _ := readView(f.layout, f.work)
	if len(after) != len(before) {
		t.Fatalf("quit must not append a journal row: %d -> %d", len(before), len(after))
	}
}

// Without a terminal, -p is refused with a guidance message.
func TestRestoreInteractiveNonTTYRefused(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_n", "x", func() { f.write(t, "f.txt", "v1") })

	oldTTY := isInteractive
	isInteractive = func() bool { return false }
	t.Cleanup(func() { isInteractive = oldTTY })

	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_n", "-p"}, &out, &errb); code != 1 {
		t.Fatalf("non-TTY -p should exit 1, got %d", code)
	}
	if !strings.Contains(errb.String(), "requires an interactive terminal; use path filters or --3way") {
		t.Fatalf("missing refusal guidance: %s", errb.String())
	}
}

// -p and --3way are mutually exclusive.
func TestRestoreInteractiveThreeWayMutex(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	f.capture(t, "tool_m", "x", func() { f.write(t, "f.txt", "v1") })

	withScript(t, "")
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_m", "-p", "--3way"}, &out, &errb); code != 1 {
		t.Fatalf("-p --3way should exit 1, got %d", code)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Fatalf("missing mutex message: %s", errb.String())
	}
}

// The overlapped gate fires before any interaction.
func TestRestoreInteractiveGateFirst(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", thirtyLines(nil))
	f.capture(t, "tool_g", "x", func() {
		f.write(t, "f.txt", thirtyLines(map[int]string{3: "X3"}))
	})
	markOverlapped(t, f, "tool_g")
	post, _ := readWork(t, f, "f.txt")

	withScript(t, "y\ny\n") // would revert if interaction were reached
	var out, errb bytes.Buffer
	if code := Restore(f.layout, f.work, []string{"tool_g", "-p"}, &out, &errb); code == 0 {
		t.Fatalf("overlapped -p without --force should refuse, got exit 0:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "overlapped") {
		t.Fatalf("missing overlapped gate message: %s", errb.String())
	}
	if v, _ := readWork(t, f, "f.txt"); v != post {
		t.Fatal("gate must fire before any work-tree change")
	}
}

func TestRestoreRefusesWhenProjectLockHeld(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v1")
	f.capture(t, "tool_lk", "x", func() { f.write(t, "f.txt", "v2") })

	if err := f.layout.EnsureRepoDirs(f.work); err != nil {
		t.Fatal(err)
	}
	lock, code := acquireProjectLock(f.layout, f.work, &bytes.Buffer{})
	if lock == nil {
		t.Fatalf("could not pre-acquire the project lock (code %d)", code)
	}
	defer func() { _ = lock.Release() }()

	var out, errb bytes.Buffer
	got := Restore(f.layout, f.work, []string{"tool_lk"}, &out, &errb)
	if got != 1 {
		t.Fatalf("exit = %d, want 1 while the lock is held", got)
	}
	if !strings.Contains(errb.String(), "another bashback process") {
		t.Fatalf("stderr = %q, want lock-contention guidance", errb.String())
	}
	if v, _ := readWork(t, f, "f.txt"); v != "v2" {
		t.Fatalf("work-tree changed under a held lock: %q", v)
	}
}
