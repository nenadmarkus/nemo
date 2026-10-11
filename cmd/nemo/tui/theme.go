package tui

import "os"

// theme holds the SGR codes every styled string routes through. When
// color is disabled every field is the empty string, so call sites stay
// branch-free.
type theme struct {
	accent, dim, muted, text, success, errc, warning, code string
	bold, italic, reset                                    string
}

// loadTheme picks the palette: NO_COLOR and TERM=dumb disable styling
// entirely.
func loadTheme() theme {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return theme{}
	}
	return theme{
		accent:  "\x1b[38;5;75m",
		dim:     "\x1b[2m",
		muted:   "\x1b[38;5;245m",
		text:    "",
		success: "\x1b[38;5;114m",
		errc:    "\x1b[38;5;203m",
		warning: "\x1b[38;5;179m",
		code:    "\x1b[38;5;109m",
		bold:    "\x1b[1m",
		italic:  "\x1b[3m",
		reset:   "\x1b[0m",
	}
}

// s wraps s in style, resetting after (a no-op when color is off).
func (t theme) s(style, s string) string {
	return style + s + t.reset
}
