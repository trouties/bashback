package daemon

import (
	"log"

	"github.com/trouties/bashback/internal/config"
	"github.com/trouties/bashback/internal/gitx"
	"github.com/trouties/bashback/internal/paths"
	"github.com/trouties/bashback/internal/snapshot"
)

// Run is the `bashback daemon run` entrypoint: clean stale sockets, bind the
// session socket, and serve until idle-exit or shutdown. ErrAlreadyRunning is
// returned (and treated as success by the caller) when another daemon owns the
// socket — lazy-start races are benign.
func Run(layout paths.Layout, sessionID string, logger *log.Logger) error {
	CleanStaleSockets(layout)
	ln, err := Listen(layout, sessionID)
	if err != nil {
		return err
	}
	eng := snapshot.New(layout, gitx.ExecRunner{})
	eng.MaxFileBytesFor = func(workdir string) int64 {
		return config.Load(layout, workdir, config.OSEnv()).MaxFileBytes
	}
	// Daemon-global TTLs come from env (no per-project meta key for them).
	cfg := config.Resolve(paths.Meta{}, config.OSEnv())
	d := New(eng, layout, sessionID, logger)
	d.StaleTTL = cfg.StaleTTL
	d.IdleTimeout = cfg.IdleTimeout
	d.Serve(ln)
	return nil
}
