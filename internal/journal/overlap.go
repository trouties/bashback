package journal

import "time"

// MarkOverlaps computes cross-session overlap on a merged view and flags it in
// place, returning the same slice. Two complete entries from different sessions
// whose [ts-duration_ms, ts] intervals intersect are both marked overlapped.
// The journal is append-only and never rewritten — this mutates only
// the in-memory read view. Stored overlap flags (same-session, daemon-detected)
// are preserved (∨ semantics); pre-only entries carry no post interval and are
// skipped, since they are refused on restore regardless.
func MarkOverlaps(entries []Entry) []Entry {
	type interval struct {
		start, end time.Time
		ok         bool
	}
	ivs := make([]interval, len(entries))
	for i, e := range entries {
		if e.PostSHA == "" {
			continue
		}
		end, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			continue
		}
		ivs[i] = interval{start: end.Add(-time.Duration(e.DurationMS) * time.Millisecond), end: end, ok: true}
	}
	for i := range entries {
		if !ivs[i].ok {
			continue
		}
		for j := i + 1; j < len(entries); j++ {
			if !ivs[j].ok || entries[i].SessionID == entries[j].SessionID {
				continue
			}
			// Strict inequalities so exactly-touching intervals do not count.
			if ivs[i].start.Before(ivs[j].end) && ivs[j].start.Before(ivs[i].end) {
				entries[i].Overlapped = true
				entries[j].Overlapped = true
			}
		}
	}
	return entries
}
