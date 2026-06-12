package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MinMajor/MinMinor is the lowest git version bashback supports: 2.32, where
// `git apply --3way` became worktree-first. doctor checks this.
const (
	MinMajor = 2
	MinMinor = 32
)

type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

// AtLeast reports whether v >= the given major.minor.
func (v Version) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// MeetsMinimum reports whether v satisfies the bashback floor.
func (v Version) MeetsMinimum() bool { return v.AtLeast(MinMajor, MinMinor) }

// DetectVersion parses `git --version`. It needs no repo, so it runs the bare
// Runner without shadow-repo flags.
func DetectVersion(ctx context.Context, r Runner) (Version, error) {
	res, err := r.Run(ctx, []string{"--version"}, RunOpts{})
	if err != nil {
		return Version{}, err
	}
	return parseVersion(string(res.Stdout))
}

// parseVersion handles "git version 2.34.1" and vendor suffixes like
// "2.39.3 (Apple Git-145)".
func parseVersion(s string) (Version, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return Version{}, fmt.Errorf("unrecognized git version output: %q", s)
	}
	raw := fields[2]
	parts := strings.SplitN(raw, ".", 4)
	v := Version{Raw: raw}
	if len(parts) > 0 {
		v.Major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		v.Minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		// Patch may carry a non-numeric suffix; take the leading digits.
		v.Patch, _ = strconv.Atoi(leadingDigits(parts[2]))
	}
	if v.Major == 0 {
		return Version{}, fmt.Errorf("could not parse git version from %q", s)
	}
	return v, nil
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return "0"
	}
	return s[:i]
}
