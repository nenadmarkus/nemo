package terminal

// Terminal cell-width measurement, integrated from the ./width program:
// text is segmented into grapheme clusters per UAX #29, and each cluster
// occupies cells decided by East Asian Width, emoji presentation,
// variation selectors, and regional-indicator pairing (see tables.go,
// generated from the official UCD). This file is the segmentation and
// width logic; it matches the behavior of grapheme-aware terminals.

import "unicode/utf8"

// Grapheme cluster break classes; must match the generated tables.
const (
	gcbOther = iota
	gcbCR
	gcbLF
	gcbControl
	gcbExtend
	gcbZWJ
	gcbRI
	gcbPrepend
	gcbSpacingMark
	gcbL
	gcbV
	gcbT
	gcbLV
	gcbLVT
)

// Property flags; must match the generated tables.
const (
	fExtPict   = 1 << 0
	fEmojiPres = 1 << 1
	fWide      = 1 << 2
	fZero      = 1 << 3
	fInCBCons  = 1 << 4
	fInCBLink  = 1 << 5
	fInCBExt   = 1 << 6
)

type prop struct {
	lo, hi     rune
	gcb, flags uint8
}

// lookup binary-searches the generated table; runes not listed have the
// default properties (gcb Other, no flags).
func lookup(r rune) prop {
	lo, hi := 0, len(props)
	for lo < hi {
		mid := (lo + hi) / 2
		p := props[mid]
		switch {
		case r < p.lo:
			hi = mid
		case r > p.hi:
			lo = mid + 1
		default:
			return p
		}
	}
	return prop{r, r, gcbOther, 0}
}

const (
	vs15 = 0xFE0E // text presentation selector
	vs16 = 0xFE0F // emoji presentation selector
)

// Rune returns the terminal cell width of a single rune considered in
// isolation: 0 for controls, combining marks, and other invisibles; 2 for
// East Asian Wide/Fullwidth and emoji-presentation characters; 1 otherwise.
// For user-visible text, prefer Next — width is a property of grapheme
// clusters, not runes.
func Rune(r rune) int {
	if r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	p := lookup(r)
	if p.flags&fZero != 0 || p.gcb == gcbExtend || p.gcb == gcbZWJ ||
		p.gcb == gcbSpacingMark || p.gcb == gcbV || p.gcb == gcbT ||
		p.gcb == gcbControl {
		return 0
	}
	if p.flags&(fWide|fEmojiPres) != 0 {
		return 2
	}
	return 1
}

// clusterWidth returns the cell width of one grapheme cluster:
//   - VS16 forces emoji presentation (2 cells); VS15 forces text (1)
//   - a regional-indicator pair (a flag) is 2
//   - pictographic clusters (emoji, incl. ZWJ sequences) are 2 when rendered
//     as emoji, 1 for text-presentation pictographs like ✳
//   - anything else sums its runes' widths, matching wcswidth
func clusterWidth(cluster string) int {
	sum, ri := 0, 0
	extPict, wide := false, false
	for _, r := range cluster {
		switch r {
		case vs16:
			return 2
		case vs15:
			return 1
		}
		p := lookup(r)
		if p.gcb == gcbRI {
			ri++
		}
		if p.flags&fExtPict != 0 {
			extPict = true
		}
		rw := Rune(r)
		sum += rw
		if rw == 2 {
			wide = true
		}
	}
	if ri >= 2 {
		return 2
	}
	if extPict {
		if wide {
			return 2
		}
		return 1
	}
	return sum
}

// Next returns the first grapheme cluster of s, its width in terminal cells,
// and the remainder of s. It returns "" when s is empty.
func Next(s string) (cluster string, cells int, rest string) {
	if s == "" {
		return "", 0, ""
	}
	end := boundaryAfter(s)
	c := s[:end]
	return c, clusterWidth(c), s[end:]
}

// String returns the number of terminal cells s occupies. ANSI escape
// sequences must not be present; see Cells for styled text.
func String(s string) int {
	total := 0
	for s != "" {
		_, w, rest := Next(s)
		total += w
		s = rest
	}
	return total
}

// Truncate cuts s at a grapheme boundary so that the result plus tail fits
// in w cells, appending tail only when a cut happened.
func Truncate(s string, w int, tail string) string {
	if String(s) <= w {
		return s
	}
	budget := w - String(tail)
	if budget < 0 {
		budget = 0
	}
	used, out := 0, 0
	for rest := s; rest != ""; {
		c, cw, r := Next(rest)
		if used+cw > budget {
			break
		}
		used += cw
		out += len(c)
		rest = r
	}
	return s[:out] + tail
}

// boundaryAfter returns the byte offset of the first grapheme boundary in
// s after the first rune, applying the UAX #29 GB rules.
func boundaryAfter(s string) int {
	r, sz := utf8.DecodeRuneInString(s)
	prev := lookup(r)
	i := sz

	// Cross-rune rule state.
	riRun := 0 // consecutive regional indicators ending at prev (GB12/13)
	if prev.gcb == gcbRI {
		riRun = 1
	}
	emoji := 0 // GB11: 1 = in ExtPict Extend*, 2 = that followed by ZWJ
	if prev.flags&fExtPict != 0 {
		emoji = 1
	}
	incbCons, incbLink := prev.flags&fInCBCons != 0, false // GB9c

	for i < len(s) {
		r, sz := utf8.DecodeRuneInString(s[i:])
		cur := lookup(r)
		if graphemeBreak(prev, cur, riRun, emoji, incbCons && incbLink) {
			return i
		}

		if cur.gcb == gcbRI {
			riRun++
		} else {
			riRun = 0
		}
		switch {
		case cur.flags&fExtPict != 0:
			emoji = 1
		case emoji == 1 && cur.gcb == gcbExtend:
			// still in ExtPict Extend*
		case emoji == 1 && cur.gcb == gcbZWJ:
			emoji = 2
		default:
			emoji = 0
		}
		switch {
		case cur.flags&fInCBCons != 0:
			incbCons, incbLink = true, false
		case incbCons && cur.flags&fInCBLink != 0:
			incbLink = true
		case incbCons && cur.flags&fInCBExt != 0:
			// linker state unchanged
		default:
			incbCons, incbLink = false, false
		}

		i += sz
		prev = cur
	}
	return len(s)
}

// graphemeBreak reports whether a boundary exists between prev and cur given
// the cross-rune state, per UAX #29 rules GB3–GB999.
func graphemeBreak(prev, cur prop, riRun, emoji int, incbLinked bool) bool {
	p, c := prev.gcb, cur.gcb
	switch {
	case p == gcbCR && c == gcbLF: // GB3
		return false
	case p == gcbControl || p == gcbCR || p == gcbLF: // GB4
		return true
	case c == gcbControl || c == gcbCR || c == gcbLF: // GB5
		return true
	case p == gcbL && (c == gcbL || c == gcbV || c == gcbLV || c == gcbLVT): // GB6
		return false
	case (p == gcbLV || p == gcbV) && (c == gcbV || c == gcbT): // GB7
		return false
	case (p == gcbLVT || p == gcbT) && c == gcbT: // GB8
		return false
	case c == gcbExtend || c == gcbZWJ: // GB9
		return false
	case c == gcbSpacingMark: // GB9a
		return false
	case p == gcbPrepend: // GB9b
		return false
	case incbLinked && cur.flags&fInCBCons != 0: // GB9c
		return false
	case emoji == 2 && cur.flags&fExtPict != 0: // GB11
		return false
	case p == gcbRI && c == gcbRI && riRun%2 == 1: // GB12/13
		return false
	}
	return true // GB999
}
