package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/paths"
)

// BenchmarkHotSnapshot10k measures steady-state per-command cost (pre fast-path +
// post snapshot of a one-file change) on a 10k-file repo. It excludes process
// cold start, which is measured separately.
func BenchmarkHotSnapshot10k(b *testing.B) {
	home := filepath.Join(b.TempDir(), ".bashback")
	work := b.TempDir()
	eng := New(paths.New(home), gitx.ExecRunner{})
	repo, err := eng.EnsureRepo(context.Background(), work, "sess")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("f%05d", i)), []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	// Establish the baseline so subsequent snapshots are incremental (hot).
	if _, err := eng.Pre(context.Background(), repo, nil); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pre, err := eng.Pre(context.Background(), repo, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := os.WriteFile(filepath.Join(work, "f00000"), []byte(fmt.Sprintf("v%d", i)), 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := eng.Post(context.Background(), repo, pre, nil); err != nil {
			b.Fatal(err)
		}
	}
}
