package tui

// stream.go: the bridge from the agent's streaming callbacks to the app
// loop. Callbacks run on other goroutines (the stream reader, tool
// handlers); they do nothing but push closures onto one buffered channel,
// which preserves their relative order — diff previews replay before
// their tool's end callback, exactly as the library emits them.

// push queues a state mutation for the app loop.
func (u *ui) push(f func()) {
	select {
	case u.agentCh <- f:
	case <-u.quit:
	}
}

// bridge returns the Run callbacks; each defers its work to the loop.
func (u *ui) bridge() (onReasoning, onContent, onSystem func(string),
	onToolStart func(callID, name, args string),
	onToolEnd func(callID, name, args string, ok bool, detail string)) {
	return func(s string) { u.push(func() { u.onReasoning(s) }) },
		func(s string) { u.push(func() { u.onContent(s) }) },
		func(s string) { u.push(func() { u.onSystem(s) }) },
		func(id, name, args string) { u.push(func() { u.onToolStart(id, name, args) }) },
		func(id, name, args string, ok bool, detail string) {
			u.push(func() { u.onToolEnd(id, name, args, ok, detail) })
		}
}
