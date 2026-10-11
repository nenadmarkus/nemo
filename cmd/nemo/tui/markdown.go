package tui

// markdown.go: md-lite renderer for finalized output blocks. Deliberately
// small — headings, fenced code, inline code/bold/italic, bullet and
// numbered lists, blockquotes, links as text — no Glamour/chroma weight.
// Pure: string in, styled wrapped lines out.

import (
	"strings"

	"nemo/terminal"
)

// renderMarkdown renders src into styled, wrapped lines at the given
// width.
func renderMarkdown(src string, width int, th theme) []string {
	var out []string
	inFence := false
	var para []string

	flushPara := func() {
		if len(para) > 0 {
			out = append(out, terminal.WrapStyled(inlineMD(strings.Join(para, " "), th), width)...)
			para = nil
		}
	}
	wrapItem := func(body string, marker string) {
		segs := terminal.WrapStyled(body, width-2)
		for i, sg := range segs {
			if i == 0 {
				out = append(out, "  "+marker+sg)
			} else {
				out = append(out, "    "+sg)
			}
		}
	}

	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			flushPara()
			inFence = !inFence
			out = append(out, th.s(th.dim, "  ┄┄"))
			continue
		}
		if inFence {
			out = append(out, terminal.WrapStyled("  "+line, width)...)
			continue
		}
		if marker, rest, ok := numbered(line); ok {
			flushPara()
			wrapItem(inlineMD(rest, th), marker+" ")
			continue
		}
		switch {
		case line == "":
			flushPara()
			if len(out) > 0 {
				out = append(out, "")
			}
		case line[0] == '#':
			flushPara()
			lvl, txt := heading(line)
			style := th.bold
			if lvl == 1 {
				style = th.accent + th.bold
			}
			out = append(out, terminal.WrapStyled(th.s(style, txt), width)...)
		case line[0] == '>':
			flushPara()
			body := th.s(th.dim, "▎ ") + inlineMD(strings.TrimSpace(strings.TrimPrefix(line, ">")), th)
			out = append(out, terminal.WrapStyled(body, width)...)
		case isBullet(line):
			flushPara()
			wrapItem(inlineMD(strings.TrimSpace(line[1:]), th), th.s(th.accent, "•")+" ")
		default:
			para = append(para, line)
		}
	}
	flushPara()
	if inFence {
		out = append(out, th.s(th.dim, "  ┄┄"))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func heading(line string) (lvl int, txt string) {
	for lvl < len(line) && line[lvl] == '#' {
		lvl++
	}
	if lvl > 3 {
		lvl = 3
	}
	return lvl, strings.TrimSpace(strings.TrimLeft(line, "#"))
}

func isBullet(line string) bool {
	return len(line) >= 2 && (line[0] == '-' || line[0] == '*') && line[1] == ' '
}

// numbered matches "1. " style list items, returning the marker and rest.
func numbered(line string) (marker, rest string, ok bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) || line[i] != '.' || line[i+1] != ' ' {
		return "", "", false
	}
	return line[:i+1], strings.TrimSpace(line[i+2:]), true
}

// inlineMD renders inline markdown: `code`, **bold**, *italic*, and
// [text](url) links (URL dropped). Unbalanced markers stay literal.
func inlineMD(s string, th theme) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString(th.code + s[i+1:i+1+j] + th.reset)
				i += j + 2
				continue
			}
		case i+1 < len(s) && s[i] == '*' && s[i+1] == '*':
			if j := strings.Index(s[i+2:], "**"); j >= 0 {
				b.WriteString(th.bold + s[i+2:i+2+j] + th.reset)
				i += j + 4
				continue
			}
		case s[i] == '*':
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString(th.italic + s[i+1:i+1+j] + th.reset)
				i += j + 2
				continue
			}
		case s[i] == '[':
			if text, n, ok := parseLink(s[i:]); ok {
				b.WriteString(text)
				i += n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// parseLink parses [text](url) at the head of s, returning the text and
// the bytes consumed.
func parseLink(s string) (text string, n int, ok bool) {
	j := strings.IndexByte(s, ']')
	if j < 0 || j+1 >= len(s) || s[j+1] != '(' {
		return "", 0, false
	}
	k := strings.IndexByte(s[j+1:], ')')
	if k < 0 {
		return "", 0, false
	}
	return s[1:j], j + k + 2, true
}
