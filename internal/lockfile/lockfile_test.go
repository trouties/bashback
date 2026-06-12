package lockfile

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireReleaseRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// Re-acquirable after release.
	l2, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestAcquireTimesOutWhileHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	held, err := Acquire(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if _, err := Acquire(p, 50*time.Millisecond); err != ErrTimeout {
		t.Fatalf("want ErrTimeout while lock held, got %v", err)
	}
}

// E4: serialization must let all contenders eventually succeed with no
// overlapping critical sections.
func TestSerializesConcurrentContenders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	const n = 10
	var active int32
	var overlaps int32
	var success int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Acquire(p, 5*time.Second)
			if err != nil {
				return
			}
			defer l.Release()
			if atomic.AddInt32(&active, 1) != 1 {
				atomic.AddInt32(&overlaps, 1)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			atomic.AddInt32(&success, 1)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&success); got != n {
		t.Fatalf("success = %d, want %d", got, n)
	}
	if got := atomic.LoadInt32(&overlaps); got != 0 {
		t.Fatalf("critical sections overlapped %d times", got)
	}
}
