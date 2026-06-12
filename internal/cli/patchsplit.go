package cli

import "strings"

// patchFile is one file's segment of a unified git diff: the header lines up to
// the first hunk, then the hunks themselves (each a "@@ " block, newlines kept so
// reassembly is byte-exact). Binary files carry the whole segment in Header and
// no hunks — they are restored whole-file (checkout/delete), never per-hunk.
type patchFile struct {
	Path   string
	Header string
	Binary bool
	Hunks  []string
}

// parsePatch splits a `git diff` patch into per-file segments and, for text
// files, per-hunk blocks. Splitting is purely structural — by the
// "diff --git " file marker and the "@@ " hunk marker — so the pieces concatenate
// back to the original bytes. A segment containing "GIT binary patch" is flagged
// Binary with its body left intact in Header.
func parsePatch(patch string) []patchFile {
	var out []patchFile
	for _, seg := range splitFileSegments(patch) {
		pf := patchFile{Path: parseDiffPath(seg)}
		if strings.Contains(seg, "GIT binary patch") {
			pf.Binary = true
			pf.Header = seg
			out = append(out, pf)
			continue
		}
		var header, cur strings.Builder
		var hunks []string
		inHunk := false
		for _, ln := range strings.SplitAfter(seg, "\n") {
			if strings.HasPrefix(ln, "@@ ") {
				if inHunk {
					hunks = append(hunks, cur.String())
					cur.Reset()
				}
				inHunk = true
				cur.WriteString(ln)
				continue
			}
			if inHunk {
				cur.WriteString(ln)
			} else {
				header.WriteString(ln)
			}
		}
		if inHunk {
			hunks = append(hunks, cur.String())
		}
		pf.Header = header.String()
		pf.Hunks = hunks
		out = append(out, pf)
	}
	return out
}

// splitFileSegments breaks a patch into segments each beginning with a
// "diff --git " line, keeping all bytes.
func splitFileSegments(patch string) []string {
	var segs []string
	var cur strings.Builder
	started := false
	for _, ln := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(ln, "diff --git ") {
			if started {
				segs = append(segs, cur.String())
				cur.Reset()
			}
			started = true
		}
		if started {
			cur.WriteString(ln)
		}
	}
	if started {
		segs = append(segs, cur.String())
	}
	return segs
}

// parseDiffPath extracts the file path from a segment's "diff --git a/P b/P"
// header line. Rename detection is off upstream, so the a/ and b/ paths match.
func parseDiffPath(seg string) string {
	first := seg
	if nl := strings.IndexByte(seg, '\n'); nl >= 0 {
		first = seg[:nl]
	}
	if i := strings.Index(first, " b/"); i >= 0 {
		return first[i+3:]
	}
	return ""
}

// assemblePatch rebuilds a patch from the selected hunks: for each file with at
// least one picked hunk, its header followed by the chosen hunks in order. Binary
// files contribute nothing here (they have no hunks and are handled whole-file).
// An all-false selector yields the empty string.
func assemblePatch(files []patchFile, picked func(fi, hi int) bool) string {
	var b strings.Builder
	for fi, f := range files {
		var chosen []string
		for hi := range f.Hunks {
			if picked(fi, hi) {
				chosen = append(chosen, f.Hunks[hi])
			}
		}
		if len(chosen) == 0 {
			continue
		}
		b.WriteString(f.Header)
		for _, h := range chosen {
			b.WriteString(h)
		}
	}
	return b.String()
}
