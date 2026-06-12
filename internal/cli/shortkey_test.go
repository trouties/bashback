package cli

import (
	"strings"
	"testing"

	"github.com/trouties/bashback/internal/journal"
)

// The sorted-neighbors implementation must agree exactly with the naive all-pairs
// definition: shortest prefix (>= 12-rune floor) unique in the set.
func TestShortKeysOfMatchesNaive(t *testing.T) {
	cases := [][]string{
		{"toolu_01AAAABBBBCCCC", "toolu_01AAAABBBBCCCD", "toolu_01XYZ"},
		{"bgfinal_toolu_01AAAABBBBCCCC", "toolu_01AAAABBBBCCCC"},
		{"manual_1718000000000", "manual_1718000000001", "k", "kk"},
		{"aaaaaaaaaaaaaaaaaa"},
		{},
	}
	for _, keys := range cases {
		got := shortKeysOf(keys)
		want := naiveShortKeys(keys)
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("keys %v: %q -> got %q want %q", keys, k, got[k], w)
			}
		}
	}
}

// naiveShortKeys is the original O(n²) definition, kept as the test oracle.
func naiveShortKeys(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		r := []rune(k)
		n := len(r)
		l := 12
		if n < l {
			l = n
		}
	scan:
		for l < n {
			for _, other := range keys {
				if other != k && strings.HasPrefix(other, string(r[:l])) {
					l++
					continue scan
				}
			}
			break
		}
		out[k] = string(r[:l])
	}
	return out
}

func entriesFromKeys(keys ...string) []journal.Entry {
	out := make([]journal.Entry, len(keys))
	for i, k := range keys {
		out[i] = journal.Entry{ToolUseID: k}
	}
	return out
}

// Distinct-prefix keys abbreviate to the 12-char floor; a sub-floor key shows full (no ellipsis ever).
func TestShortKeysFloor(t *testing.T) {
	short := shortKeys(entriesFromKeys("aaaa_longkey_1234567", "bbbb_longkey_1234567", "snap_xy"))
	if got := short["aaaa_longkey_1234567"]; got != "aaaa_longkey" {
		t.Fatalf("floor key = %q, want %q", got, "aaaa_longkey")
	}
	if got := short["bbbb_longkey_1234567"]; got != "bbbb_longkey" {
		t.Fatalf("floor key = %q, want %q", got, "bbbb_longkey")
	}
	if got := short["snap_xy"]; got != "snap_xy" {
		t.Fatalf("sub-floor key should show full, got %q", got)
	}
	for _, v := range short {
		if strings.Contains(v, "…") {
			t.Fatalf("short key %q must not contain an ellipsis", v)
		}
	}
}

// Two keys sharing more than the floor's prefix extend until the first divergent char is included.
func TestShortKeysExtendOnCollision(t *testing.T) {
	a, b := "toolu_aaaaaaaa_x", "toolu_aaaaaaaa_y"
	short := shortKeys(entriesFromKeys(a, b))
	if short[a] == short[b] {
		t.Fatalf("collision not broken: both = %q", short[a])
	}
	// Shared prefix is "toolu_aaaaaaaa_" (15 chars); divergence at index 15, so the
	// short key must be at least 16 chars to include it.
	if len(short[a]) < 16 || len(short[b]) < 16 {
		t.Fatalf("short keys too short to disambiguate: %q / %q", short[a], short[b])
	}
	if !strings.HasPrefix(a, short[a]) || !strings.HasPrefix(b, short[b]) {
		t.Fatalf("short keys must be true prefixes: %q / %q", short[a], short[b])
	}
}

// A bgfinal key abbreviates on the full key, so the bgfinal_ marker survives.
func TestShortKeysBgfinal(t *testing.T) {
	k := "bgfinal_toolu_xyz1234567890"
	short := shortKeys(entriesFromKeys(k, "toolu_other_abcdefg"))
	if !strings.HasPrefix(short[k], "bgfinal_") {
		t.Fatalf("bgfinal short key lost its marker: %q", short[k])
	}
}

// Property: every short key resolves back to its own entry through the real
// addressing path — "displayed is usable" is a hard guarantee.
func TestShortKeysResolvable(t *testing.T) {
	f := newFix(t)
	f.write(t, "f.txt", "v0")
	ids := []string{
		"toolu_aaaaaaaa_first",
		"toolu_aaaaaaaa_second",
		"bgfinal_toolu_bbbbbbbb_x",
		"toolu_cccccccc_solo",
		"snap_short",
	}
	for i, id := range ids {
		i, id := i, id
		f.capture(t, id, "cmd", func() { f.write(t, "f.txt", id) })
		_ = i
	}
	entries, err := readView(f.layout, f.work)
	if err != nil {
		t.Fatal(err)
	}
	short := shortKeys(entries)
	for _, e := range entries {
		full := journal.DefaultKeyer.Key(e)
		sk := short[full]
		if strings.Contains(sk, "…") {
			t.Fatalf("short key %q has ellipsis", sk)
		}
		got, ok, rerr := resolveEntry(f.layout, f.work, sk)
		if rerr != nil || !ok {
			t.Fatalf("short key %q for %q did not resolve: ok=%v err=%v", sk, full, ok, rerr)
		}
		if journal.DefaultKeyer.Key(got) != full {
			t.Fatalf("short key %q resolved to %q, want %q", sk, journal.DefaultKeyer.Key(got), full)
		}
	}
}
