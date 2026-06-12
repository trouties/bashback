//go:build unix

package journal

import (
	"encoding/json"
	"os"
	"strings"
	"syscall"
)

// Repair moves unparseable journal lines into journal.bad beside the ledger
// (append; the audit trail is never deleted) and rewrites the journal with the
// surviving lines via tmp+rename under the append flock. It returns how many
// lines were quarantined. A clean or absent journal is a no-op.
func Repair(path string) (moved int, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var good, bad []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) == nil && e.V <= SchemaVersion {
			good = append(good, line)
			continue
		}
		bad = append(bad, line)
	}
	if len(bad) == 0 {
		return 0, nil
	}

	if err := appendLines(path+".bad", bad); err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(joinLines(good)), 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return len(bad), nil
}

func appendLines(path string, lines []string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(joinLines(lines))
	return err
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
