// Package tui is nemo's terminal UI (see tui-plan.md): a streaming agent
// transcript owned by the terminal's scrollback plus a repainted tail
// with the live editor. The engine is untouched — the TUI replaces the
// sink of Agent.Run's callbacks, not the agent; turn lifecycle, Ctrl-C
// abort/rollback, and usage reporting port the plain REPL 1:1.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nemo"
	"nemo/terminal"
)

const (
	ownedTail = 20                     // streaming-block lines shown in the owned region
	tickRate  = 100 * time.Millisecond // spinner + size poll
)

// ui holds all TUI state; it is mutated only by the loop goroutine.
type ui struct {
	ag      *nemo.Agent
	s       *nemo.Session
	t       *terminal.Terminal
	p       *terminal.Painter
	ed      *terminal.Editor
	th      theme
	keys    chan terminal.Key
	agentCh chan func()
	quit    chan struct{}
	width   int

	cur   *block   // open streaming block (thinking/output)
	tools []*block // open tool blocks
	dirty bool
	spin  int

	running   bool
	unwinding bool
	cancel    context.CancelFunc
	idleCtrl  int
	exiting   bool
}

// Run drives the TUI for one agent and session; the terminal is restored
// on return and on panic (the deferred Restore covers unwinding).
func Run(ag *nemo.Agent) (err error) {
	t, err := terminal.Open()
	if err != nil {
		return err
	}
	defer t.Restore()
	u := &ui{
		ag:      ag,
		s:       nemo.NewSession(),
		t:       t,
		p:       terminal.NewPainter(os.Stdout),
		ed:      terminal.NewEditor(fileCompleter{}),
		th:      loadTheme(),
		keys:    make(chan terminal.Key, 8),
		agentCh: make(chan func(), 256),
		quit:    make(chan struct{}),
	}
	u.width, _ = t.Size()
	return u.loop()
}

func (u *ui) loop() error {
	go terminal.ReadKeys(os.Stdin, u.keys, u.quit)

	u.p.Finalize([]string{
		u.th.s(u.th.accent+u.th.bold, "nemo") + u.th.s(u.th.dim,
			": turn-based coding assistant"),
		u.th.s(u.th.dim, "Enter submit · Ctrl+J newline · Ctrl+C aborts turn · Ctrl+D exits"),
		u.th.s(u.th.dim, fmt.Sprintf("nemo: model=%s base=%s", u.ag.Model, u.ag.Endpoint)),
	})
	u.repaint()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	tick := time.NewTicker(tickRate)
	defer tick.Stop()

	for !u.exiting {
		select {
		case k := <-u.keys:
			u.onKey(k)
		case f := <-u.agentCh:
			f()
		case <-tick.C:
			// Resize is handled by polling: SIGWINCH is Unix-only and a
			// 100ms ioctl is negligible.
			if w, _ := u.t.Size(); w != u.width {
				u.width = w
				u.dirty = true
			}
			if u.running {
				u.spin++
				u.dirty = true
			}
		case <-sig:
			if u.running && u.cancel != nil {
				u.cancel()
			} else {
				u.exiting = true
			}
		}
		if u.dirty {
			u.repaint()
		}
	}
	close(u.quit)
	u.p.Finalize([]string{""})
	return nil
}

// onKey handles one keypress: interrupts, quit paths, and the editor.
func (u *ui) onKey(k terminal.Key) {
	switch {
	case k.Name == terminal.KeyRune && k.Ctrl && k.R == 'c':
		// In raw mode Ctrl-C arrives in-band — no race with the tty's
		// line discipline.
		u.interrupt()
		return
	case k.Name == terminal.KeyRune && k.Ctrl && k.R == 'd' && u.ed.Empty():
		u.exiting = true
		return
	case k.Name == terminal.KeyRune && k.Ctrl && k.R == 'l':
		// Clear the screen but keep the prompt (and any streaming state).
		u.p.ClearScreen()
		u.repaint()
		return
	case k.Name == terminal.KeyEsc && u.running:
		u.interrupt()
		return
	}
	u.idleCtrl = 0
	if u.running && k.Name == terminal.KeyEnter && !k.Alt {
		return // type-ahead: editing stays live, submitting waits
	}
	if u.ed.Key(k) {
		text := strings.TrimSpace(u.ed.Text())
		u.ed.Reset()
		switch {
		case text == "":
		case text == "exit" || text == "quit":
			u.exiting = true
		default:
			u.ed.Add(text)
			u.startTurn(text)
		}
	}
	u.dirty = true
}

// interrupt implements the Ctrl-C semantics ported from the plain REPL:
// during a run it cancels the turn (a second press while unwinding
// quits); at the prompt a double press exits.
func (u *ui) interrupt() {
	if u.running {
		if u.unwinding {
			u.exiting = true
			return
		}
		u.cancel()
		u.unwinding = true
		u.systemNotice("aborting turn… (Ctrl+C again to quit)")
		return
	}
	u.idleCtrl++
	if u.idleCtrl >= 2 {
		u.exiting = true
		return
	}
	u.systemNotice(`(to quit: Ctrl+D or "exit"; during a run, Ctrl+C aborts the turn)`)
}

// startTurn echoes the user input, finalizing any open state, and runs
// one exchange on its own context: a 60-minute turn timeout bounds it
// and Ctrl-C can cancel it without touching the session or the process.
// The display emitter rides in the context so write/edit print diffs
// without a special result channel.
func (u *ui) startTurn(input string) {
	u.finalizeBlock(newBlock(bUser, input))
	u.running = true
	u.unwinding = false
	before := len(u.s.Messages)
	uBefore := u.s.Usage

	baseCtx := nemo.WithDisplay(context.Background(), func(d string) {
		u.push(func() { u.onDiff(d) })
	})
	turnCtx, cancel := context.WithTimeout(baseCtx, 60*time.Minute)
	u.cancel = cancel
	onR, onC, onS, onTS, onTE := u.bridge()
	go func() {
		err := u.ag.Run(turnCtx, u.s, input, onR, onC, onS, onTS, onTE)
		cancel()
		u.push(func() { u.endTurn(err, before, uBefore) })
	}()
}

// endTurn closes the turn: dangling open blocks are finalized, a failed
// or interrupted run is rolled back out of the history (partial exchanges
// stay invalid), and the usage delta is reported.
func (u *ui) endTurn(err error, before int, uBefore nemo.Usage) {
	unwound := u.unwinding
	u.running = false
	u.unwinding = false
	u.cancel = nil
	u.closeCur()
	for _, tb := range u.tools {
		tb.ok = false
		tb.detail = "(interrupted)"
		u.finalizeBlock(tb)
	}
	u.tools = nil
	if err != nil {
		u.s.Messages = u.s.Messages[:before]
		if errors.Is(err, context.Canceled) {
			u.finalizeBlock(newBlock(bSystem, "interrupted; turn dropped"))
		} else {
			u.finalizeBlock(newBlock(bSystem, "error: "+err.Error()))
		}
	} else if unwound {
		// Race: the turn completed before the cancel took effect.
		u.finalizeBlock(newBlock(bSystem, "turn completed before interrupt; kept"))
	}
	if turn := u.s.Usage.Delta(uBefore); turn.PromptTokens > 0 || turn.CompletionTokens > 0 {
		u.finalizeBlock(newBlock(bUsage, fmt.Sprintf("[usage] turn: %s | session: %s", turn, u.s.Usage)))
	}
	u.dirty = true
}

// --- agent-side event handlers (run on the loop via bridge closures) ---

func (u *ui) onReasoning(s string) {
	if u.cur == nil || u.cur.kind != bThinking {
		u.closeCur()
		u.cur = &block{kind: bThinking}
	}
	u.cur.body.WriteString(s)
	u.dirty = true
}

func (u *ui) onContent(s string) {
	if u.cur == nil || u.cur.kind != bOutput {
		u.closeCur()
		u.cur = &block{kind: bOutput}
	}
	u.cur.body.WriteString(s)
	u.dirty = true
}

func (u *ui) onSystem(s string) {
	u.finalizeBlock(newBlock(bSystem, s))
}

func (u *ui) onToolStart(id, name, args string) {
	u.closeCur()
	label := name
	if id != "" {
		label = id + " " + name
	}
	u.tools = append(u.tools, &block{kind: bTool, id: id, label: label, args: args})
	u.dirty = true
}

func (u *ui) onToolEnd(id, name, args string, ok bool, detail string) {
	for i, tb := range u.tools {
		if tb.id == id || (id == "" && tb.label == name) {
			u.tools = append(u.tools[:i], u.tools[i+1:]...)
			tb.ok, tb.detail = ok, detail
			u.finalizeBlock(tb)
			return
		}
	}
	// End without a matching start (e.g. events dropped at shutdown).
	label := name
	if id != "" {
		label = id + " " + name
	}
	u.finalizeBlock(&block{kind: bTool, id: id, label: label, args: args, ok: ok, detail: detail})
}

func (u *ui) onDiff(s string) {
	u.finalizeBlock(newBlock(bDiff, s))
}

func (u *ui) closeCur() {
	if u.cur != nil {
		u.finalizeBlock(u.cur)
		u.cur = nil
	}
}

func (u *ui) finalizeBlock(b *block) {
	u.p.Finalize(b.renderFinal(u))
	u.dirty = true
}

func (u *ui) systemNotice(text string) {
	u.finalizeBlock(newBlock(bSystem, text))
}

// --- rendering ---

func (u *ui) repaint() {
	u.dirty = false
	lines, erow, ecol := u.owned()
	u.p.Repaint(lines)
	u.p.Cursor(erow, ecol)
}

// owned composes the owned region: the streaming block's dim tail, open
// tool rows, the spinner, the completion ghost line, then the editor.
func (u *ui) owned() (lines []string, curRow, curCol int) {
	w := u.width - 1
	th := u.th
	if u.cur != nil {
		if u.cur.kind == bThinking {
			lines = append(lines, th.s(th.dim+th.italic, "▍ thinking"))
		}
		for _, l := range tailLines(u.cur.body.String(), ownedTail) {
			lines = append(lines, terminal.WrapStyled(th.s(th.dim, l), w)...)
		}
	}
	for _, tb := range u.tools {
		args := nemo.TruncateUTF8(strings.ReplaceAll(tb.args, "\n", " "), 96, " ...")
		line := th.s(th.accent, "⋯ [tool: "+tb.label+"]") + " " + th.s(th.muted, args)
		lines = append(lines, terminal.WrapStyled(line, w)...)
	}
	if u.running {
		status := terminal.Frame(u.spin) + " running…"
		if u.unwinding {
			status = "aborting…"
		}
		lines = append(lines, th.s(th.dim, status))
	}
	if pend := u.ed.Pending(); len(pend) > 0 {
		shown := pend
		if len(shown) > 10 {
			shown = append(append([]string{}, shown[:10]...), fmt.Sprintf("+%d more", len(pend)-10))
		}
		lines = append(lines, terminal.WrapStyled(th.s(th.dim, "  "+strings.Join(shown, "   ")), w)...)
	}
	edLines, erow, ecol := u.ed.Render(u.width)
	base := len(lines)
	lines = append(lines, edLines...)
	return lines, base + erow, ecol
}
