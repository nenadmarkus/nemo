package terminal

// complete.go: the completion hook the editor calls on Tab. The
// implementation lives in the app layer.

// Completer suggests completions for the token ending at the cursor.
// Candidates are suffixes to insert for the current token, in display
// order.
type Completer interface {
	Complete(line string, cursor int) (candidates []string)
}
