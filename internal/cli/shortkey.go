package cli

import (
	"sort"

	"github.com/trouties/bashback/internal/journal"
)

// shortKeys maps each entry's full key to its shortest unique abbreviation:
// the shortest prefix (never below a 12-rune floor) that no *other* key in the
// set shares. A key shorter than the floor is returned whole. The result is
// always resolvable; no ellipsis is ever introduced — "displayed is usable".
func shortKeys(entries []journal.Entry) map[string]string {
	var keys []string
	for _, e := range entries {
		if k := journal.DefaultKeyer.Key(e); k != "" {
			keys = append(keys, k)
		}
	}
	return shortKeysOf(keys)
}

// shortKeysOf computes the abbreviation map over raw keys. Sorting once makes
// each key's closest prefix-competitors its sorted neighbors, so the shortest
// unique prefix needs two adjacent comparisons instead of an all-pairs scan
// (O(n log n) vs O(n²)).
func shortKeysOf(keys []string) map[string]string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	out := make(map[string]string, len(keys))
	for i, k := range sorted {
		need := 12 // display floor
		if i > 0 {
			if n := commonPrefixLen(k, sorted[i-1]) + 1; n > need {
				need = n
			}
		}
		if i+1 < len(sorted) {
			if n := commonPrefixLen(k, sorted[i+1]) + 1; n > need {
				need = n
			}
		}
		r := []rune(k)
		if need > len(r) {
			need = len(r)
		}
		out[k] = string(r[:need])
	}
	return out
}

// commonPrefixLen returns the rune length of the longest common prefix.
func commonPrefixLen(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return n
}
