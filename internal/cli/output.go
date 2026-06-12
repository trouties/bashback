package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// outputVersion is the --json output schema version. Like the journal row
// version, additive changes do not bump it; it only rises on an incompatible
// shape change.
const outputVersion = 1

// popJSONFlag pulls a bare `--json` out of an argument list (commands without a
// flag.FlagSet use this); it returns whether it was present and the remaining
// args. Commands that own a FlagSet register `--json` there instead.
func popJSONFlag(args []string) (bool, []string) {
	return popFlag(args, "--json")
}

// atomicWriteFile writes data to path via a same-dir tmp file and rename, so a
// concurrent reader (or a crash mid-write) never observes a half-written
// settings file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// popFlag removes a single boolean flag from args, reporting whether it was
// present. Both --flag and -flag spellings are accepted.
func popFlag(args []string, name string) (bool, []string) {
	short := strings.TrimPrefix(name, "-")
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == name || a == "-"+short {
			found = true
			continue
		}
		out = append(out, a)
	}
	return found, out
}

// emitJSON writes v as the single top-level JSON object and returns 0. v is
// expected to carry a `"v"` schema field. Marshaling failure is reported on
// stderr with a non-zero code so scripts still see the error contract.
func emitJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errf(stderr, "encode json: %v", err)
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}
