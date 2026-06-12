package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// protectHarness is a harness whose engine resolves a fixed protect_paths list,
// mirroring the production wiring (ProtectPathsFor from config).
func protectHarness(t *testing.T, dirs ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.e.ProtectPathsFor = func(string) []string { return dirs }
	return h
}

// Only files under a protected root are captured; everything else is excluded
// and, critically, never dirties status so the fast-path keeps hitting (E7).
func TestProtectPathsOnlyCapturesManifest(t *testing.T) {
	h := protectHarness(t, "keep")
	h.write(t, "keep/a.txt", "v1")
	h.write(t, "other/b.txt", "v1")

	first := h.capture(t, nil, func() {
		h.write(t, "keep/a.txt", "v2")
		h.write(t, "other/b.txt", "v2")
	})

	var paths []string
	for _, f := range first.Files {
		paths = append(paths, f.P)
	}
	for _, p := range paths {
		if filepath.Dir(p) == "other" {
			t.Fatalf("unprotected file leaked into snapshot: %v", paths)
		}
	}
	foundKeep := false
	for _, p := range paths {
		if p == "keep/a.txt" {
			foundKeep = true
		}
	}
	if !foundKeep {
		t.Fatalf("protected file not captured: %v", paths)
	}
}

// E7 hard assertion: with sparse protection on, a change to a file OUTSIDE the
// manifest leaves the work-tree clean from the shadow repo's view, so Pre takes
// the fast-path (status --porcelain is empty).
func TestProtectPathsFastPathStaysHit(t *testing.T) {
	h := protectHarness(t, "keep")
	h.write(t, "keep/a.txt", "v1")
	h.capture(t, nil, nil) // baseline commit

	// Mutate only unprotected files.
	h.write(t, "other/b.txt", "changed")
	h.write(t, "noise.log", "noise")

	repo, err := h.e.EnsureRepo(ctx(), h.work, h.session)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := repo.Clean(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("unprotected changes must not dirty the shadow status (E7 fast-path regression)")
	}
	pre, err := h.e.Pre(ctx(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pre.Skipped {
		t.Fatal("Pre should fast-path skip when only unprotected files changed")
	}
}

// A nested protect root re-includes its full parent chain so git can descend.
func TestProtectPathsNestedRoot(t *testing.T) {
	h := protectHarness(t, "src/app")
	h.write(t, "src/app/main.go", "v1")
	h.write(t, "src/vendor/lib.go", "v1") // sibling under src, NOT protected
	h.write(t, "README.md", "top")

	e := h.capture(t, nil, func() {
		h.write(t, "src/app/main.go", "v2")
		h.write(t, "src/vendor/lib.go", "v2")
		h.write(t, "README.md", "edited")
	})

	for _, f := range e.Files {
		if f.P != "src/app/main.go" {
			t.Fatalf("only src/app should be captured, got %s", f.P)
		}
	}
}

// force_include wins over sparse exclusion: a force-included file outside any
// protected root is still captured (negation patterns lose to `add -f`).
func TestProtectPathsForceIncludeWins(t *testing.T) {
	h := protectHarness(t, "keep")
	h.write(t, "keep/a.txt", "v1")
	h.write(t, "secret.env", "TOKEN=1")

	e := h.capture(t, []string{"secret.env"}, func() {
		h.write(t, "keep/a.txt", "v2")
		h.write(t, "secret.env", "TOKEN=2")
	})

	found := false
	for _, f := range e.Files {
		if f.P == "secret.env" {
			found = true
		}
	}
	if !found {
		t.Fatalf("force_include should override sparse protection: %+v", e.Files)
	}
}

// The exclude file carries the blanket `*` plus per-root negations, in that order.
func TestProtectPathsWritesSparsePatterns(t *testing.T) {
	h := protectHarness(t, "keep")
	if _, err := h.e.EnsureRepo(ctx(), h.work, h.session); err != nil {
		t.Fatal(err)
	}
	gitDir := h.e.Layout.SessionGitDir(h.work, h.session)
	b, err := os.ReadFile(filepath.Join(gitDir, "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	has := func(want string) bool {
		for _, l := range lines {
			if l == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"*", "!/keep", "!/keep/**"} {
		if !has(want) {
			t.Fatalf("exclude missing %q; got:\n%s", want, string(b))
		}
	}
}
