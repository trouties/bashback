package gitx

import (
	"context"
	"sync"
)

// FakeRunner records every invocation and delegates behavior to Func, letting
// downstream packages drive gitx without real git. It is concurrency-safe so it
// can back the daemon's serialization tests under -race.
type FakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// Func produces the result for a given invocation. If nil, every call
	// returns an empty success.
	Func func(args []string, opts RunOpts) (Result, error)
}

func (f *FakeRunner) Run(_ context.Context, args []string, opts RunOpts) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, args...))
	fn := f.Func
	f.mu.Unlock()
	if fn == nil {
		return Result{}, nil
	}
	return fn(args, opts)
}

// Calls returns a copy of every recorded invocation's args.
func (f *FakeRunner) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}
