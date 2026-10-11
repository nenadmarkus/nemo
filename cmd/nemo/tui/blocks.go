package tui

// blocks.go: the transcript block model. A block is streamed dim/plain in
// the owned region while open, then finalized with its pretty renderer
// (markdown, colors, diff marks) into scrollback.

import (
	"fmt"
	"strings"

	"nemo"
	"nemo/terminal"
)

type blockKind int

const (
	bUser blockKind = iota
	bThinking
	bOutput
	bTool
	bDiff
	bSystem
	bUsage
)

// block is one transcript unit.
type block struct {
	kind   blockKind
	id     string // tool call id
	label  string // tool label ("id name", like the plain REPL)
	args   string // tool arguments one-liner
	detail string // tool result detail
	ok     bool
	body   strings.Builder
}

// newBlock builds a block of kind k carrying text in its body. (The body
// is written after construction: a used strings.Builder must not be
// copied.)
func newBlock(k blockKind, text string) *block {
	b := &block{kind: k}
	b.body.WriteString(text)
	return b
}

// renderFinal produces the finalized, styled, wrapped lines.
func (b *block) renderFinal(u *ui) []string {
	w := u.width - 1
	th := u.th
	switch b.kind {
	case bUser:
		var out []string
		for i, l := range strings.Split(strings.TrimRight(b.body.String(), "\n"), "\n") {
			p := th.s(th.accent+th.bold, "› ")
			if i > 0 {
				p = th.s(th.accent+th.bold, "  ")
			}
			out = append(out, terminal.WrapStyled(p+l, w)...)
		}
		return out
	case bThinking:
		out := []string{th.s(th.dim+th.italic, "▍ thinking")}
		for _, l := range strings.Split(strings.TrimRight(b.body.String(), "\n"), "\n") {
			out = append(out, terminal.WrapStyled(th.s(th.dim+th.italic, l), w)...)
		}
		return out
	case bOutput:
		return renderMarkdown(b.body.String(), w, th)
	case bTool:
		return u.renderTool(b, true)
	case bDiff:
		return renderDiff(b.body.String(), w, th)
	case bSystem:
		oneLine := strings.ReplaceAll(strings.TrimSpace(b.body.String()), "\n", " ")
		return terminal.WrapStyled(th.s(th.warning, "[system] ")+th.s(th.dim, oneLine), w)
	case bUsage:
		return terminal.WrapStyled(th.s(th.dim, b.body.String()), w)
	}
	return nil
}

// renderTool renders a tool block: header, args line, and — when final —
// the ok/error status plus truncated detail.
func (u *ui) renderTool(b *block, final bool) []string {
	th := u.th
	w := u.width - 1
	hdrStyle := th.success
	if !final {
		hdrStyle = th.accent
	} else if !b.ok {
		hdrStyle = th.errc
	}
	var out []string
	out = append(out, terminal.WrapStyled(th.s(hdrStyle+th.bold, "[tool: "+b.label+"]"), w)...)
	if args := nemo.TruncateUTF8(strings.ReplaceAll(b.args, "\n", " "), 128, " ..."); args != "" {
		out = append(out, terminal.WrapStyled(th.s(th.muted, args), w)...)
	}
	if final {
		status := "ok:"
		lim := 128
		if !b.ok {
			status = "error:"
			lim = 512
		}
		out = append(out, terminal.WrapStyled(th.s(hdrStyle, status), w)...)
		detail := strings.ReplaceAll(b.detail, "\n", " ⏎ ")
		out = append(out, terminal.WrapStyled(th.s(th.dim, nemo.TruncateUTF8(detail, lim, " ...")), w)...)
	}
	return out
}

// renderDiff colors a diffLines-style block: header accent-bold, '+'
// green, '-' red, context dim.
func renderDiff(s string, width int, th theme) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		style := th.dim
		switch {
		case strings.HasPrefix(l, "[diff:"):
			style = th.accent + th.bold
		case strings.HasPrefix(l, "+"):
			style = th.success
		case strings.HasPrefix(l, "-"):
			style = th.errc
		}
		out = append(out, terminal.WrapStyled(th.s(style, terminal.Truncate(l, width, " …")), width)...)
	}
	return out
}

// tailLines returns the last n logical lines of s, prefixed with an
// overflow indicator when truncated (the owned region can only repaint
// its tail).
func tailLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		rest := append([]string{fmt.Sprintf("… +%d lines above", len(lines)-n)}, lines[len(lines)-n:]...)
		return rest
	}
	return lines
}
