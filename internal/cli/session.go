package cli

import (
	"sort"
	"time"

	"github.com/trouties/bashback/internal/journal"
)

// multiSessionWindow is how recently a session must have written a non-manual
// entry to count as active for the undo surprise gate.
const multiSessionWindow = time.Hour

// sessionActivity summarizes one session's most recent real (non-manual) entry,
// used to render the undo gate's active-session list.
type sessionActivity struct {
	ID          string
	LastTS      string
	LastCommand string
}

// activeSessions returns the distinct sessions with a non-manual entry inside the
// multi-session window, newest activity first. Manual snapshots do not count as
// activity.
func activeSessions(entries []journal.Entry, now time.Time) []sessionActivity {
	latest := map[string]sessionActivity{}
	// tsDescOrder puts the newest entry first, so the first non-manual entry seen
	// per session is its latest activity.
	for _, e := range tsDescOrder(entries) {
		if e.Status == journal.StatusManual || e.SessionID == "" {
			continue
		}
		if _, seen := latest[e.SessionID]; seen {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.TS)
		if err != nil || now.Sub(t) >= multiSessionWindow {
			continue
		}
		latest[e.SessionID] = sessionActivity{ID: e.SessionID, LastTS: e.TS, LastCommand: e.Command}
	}
	out := make([]sessionActivity, 0, len(latest))
	for _, a := range latest {
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out
}

// shortSession renders a session id as its first 8 characters (uuid prefix), or
// the whole id when shorter.
func shortSession(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
