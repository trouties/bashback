package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingWriterRollsAtSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "bashback.log")
	w, err := NewRotatingWriter(path, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Three 10-byte writes: the third trips the 20-byte cap and rotates.
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != 10 {
		t.Fatalf("active log = %d bytes, want 10 (post-rotation)", len(cur))
	}
}
