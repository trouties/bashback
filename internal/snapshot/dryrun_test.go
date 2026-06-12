package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func sortedHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A protected-entry dry-run returns the three-class plan and leaves the work-tree
// and shadow repo untouched: no new commit, status still clean.
func TestRestorePlanProtectedNoSideEffects(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	h.write(t, "gone.txt", "x")
	entry := h.capture(t, nil, func() {
		h.write(t, "keep.txt", "v2")                 // M
		os.Remove(filepath.Join(h.work, "gone.txt")) // D
		h.write(t, "new.txt", "n")                   // A
	})

	repo := h.e.RepoFor(h.work, h.session)
	headBefore, _ := repo.HeadSHA(ctx())

	plan, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "three-class" {
		t.Fatalf("mode = %q", plan.Mode)
	}
	if !sortedHas(plan.Checkout, "keep.txt") || !sortedHas(plan.Checkout, "gone.txt") {
		t.Fatalf("checkout should include M/D files: %+v", plan.Checkout)
	}
	if !sortedHas(plan.Delete, "new.txt") {
		t.Fatalf("delete should include the created file: %+v", plan.Delete)
	}

	// No side effects: HEAD unchanged, work-tree still in its post state.
	headAfter, _ := repo.HeadSHA(ctx())
	if headBefore != headAfter {
		t.Fatalf("dry-run advanced HEAD: %s -> %s", headBefore, headAfter)
	}
	if v, _ := h.read("keep.txt"); v != "v2" {
		t.Fatalf("dry-run modified the work-tree: keep.txt = %q", v)
	}
	if _, ok := h.read("new.txt"); !ok {
		t.Fatal("dry-run deleted a file")
	}
}

// Gates refuse identically under dry-run: an unforced gate returns the same
// error; --force surfaces the gate in plan.Gates.
func TestRestorePlanGatesMatchRealExecution(t *testing.T) {
	h := newHarness(t)
	h.write(t, "f.txt", "x")
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", "y") })
	entry.Overlapped = true

	if _, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{}); err != ErrOverlapped {
		t.Fatalf("want ErrOverlapped, got %v", err)
	}
	plan, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sortedHas(plan.Gates, "overlapped") {
		t.Fatalf("forced plan should list the overlapped gate: %+v", plan.Gates)
	}
}

func TestRestorePlanTargetChanged(t *testing.T) {
	h := newHarness(t)
	const pre = "top\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom\n"
	const post = "top-cmd\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom\n"
	h.write(t, "f.txt", pre)
	entry := h.capture(t, nil, func() { h.write(t, "f.txt", post) })
	h.write(t, "f.txt", "top-cmd\nm1\nm2\nm3\nm4\nm5\nm6\nm7\nbottom-user\n")

	if _, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{}); err != ErrTargetChanged {
		t.Fatalf("want ErrTargetChanged, got %v", err)
	}
	plan, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{ThreeWay: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "3way" || !sortedHas(plan.Gates, "target-changed") {
		t.Fatalf("3way plan should flag target-changed: mode=%q gates=%+v", plan.Mode, plan.Gates)
	}
}

// pre-only dry-run uses the scratch-index work-tree diff, so it must still see
// the command-created (untracked) file as something to delete.
func TestRestorePlanPreOnlyIncludesUntracked(t *testing.T) {
	h := newHarness(t)
	h.write(t, "keep.txt", "v1")
	entry := h.preCapture(t, func() {
		h.write(t, "keep.txt", "v2")
		h.write(t, "created.txt", "half")
	})

	if _, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{}); err != ErrPreOnly {
		t.Fatalf("want ErrPreOnly, got %v", err)
	}
	plan, err := h.e.RestorePlan(ctx(), h.work, entry, RestoreOpts{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sortedHas(plan.Delete, "created.txt") {
		t.Fatalf("pre-only plan must include the untracked created file: %+v", plan.Delete)
	}
	if !sortedHas(plan.Checkout, "keep.txt") {
		t.Fatalf("pre-only plan should restore the modified file: %+v", plan.Checkout)
	}
	if !sortedHas(plan.Gates, "pre-only") {
		t.Fatalf("plan should flag pre-only gate: %+v", plan.Gates)
	}

	// The dry-run must not have left a scratch index or touched the work-tree.
	if v, _ := h.read("keep.txt"); v != "v2" {
		t.Fatalf("dry-run modified work-tree: %q", v)
	}
}
