package terminal

// spinner.go: the running-turn indicator frames (braille dots).

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Frame returns the i-th spinner frame, wrapping on overflow.
func Frame(i int) string {
	n := len(spinnerFrames)
	return spinnerFrames[((i%n)+n)%n]
}
