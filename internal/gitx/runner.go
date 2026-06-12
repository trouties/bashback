// Package gitx is the only package that shells out to real git. Everything else
// depends on its interface so it can be faked. All shadow-repo commits inject a
// fixed identity and disable gpg signing: the shadow repo must never
// depend on, or be perturbed by, the user's git config.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes one git invocation. It is the single seam tests fake.
type Runner interface {
	Run(ctx context.Context, args []string, opts RunOpts) (Result, error)
}

type RunOpts struct {
	// Dir is the process working directory. For shadow-repo commands the
	// work-tree is passed via --work-tree instead, so Dir is usually unset.
	Dir string
	// Stdin is fed to git's standard input (e.g. patches for `apply`).
	Stdin []byte
	// Env entries are appended to the isolated base environment.
	Env []string
}

type Result struct {
	Stdout []byte
	Stderr []byte
	Code   int
}

// ExitError reports a git invocation that ran but exited non-zero. Callers
// inspect Code/Stderr to distinguish "1 == differences found" from real faults.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, e.Stderr)
}

// ExecRunner is the production Runner. It isolates git from the user's global
// and system config so shadow-repo behavior is deterministic, then relies on
// per-command `-c` flags for the bashback identity.
type ExecRunner struct {
	// GitBin overrides the git executable (tests). Defaults to "git".
	GitBin string
}

func (r ExecRunner) bin() string {
	if r.GitBin != "" {
		return r.GitBin
	}
	return "git"
}

func (r ExecRunner) Run(ctx context.Context, args []string, opts RunOpts) (Result, error) {
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Dir = opts.Dir
	cmd.Env = append(isolatedEnv(), opts.Env...)
	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
			return res, &ExitError{Args: args, Code: res.Code, Stderr: stderr.String()}
		}
		return res, err // failed to start git at all (not found, ctx cancelled, …)
	}
	return res, nil
}

// isolatedEnv inherits PATH and friends but pins git config to /dev/null so the
// shadow repo ignores the user's global/system config (excludesfile, signing,
// identity). The bashback identity is injected per-command via `-c`.
func isolatedEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		// Never prompt for credentials/GPG from a hook.
		"GIT_TERMINAL_PROMPT=0",
	)
	return env
}
