package tui

// complete.go: Tab file-path completion for the editor. Deliberately not
// gitignore-aware (the agent's find/ls tools already filter; completion
// is a convenience).

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const completeCap = 20

// fileCompleter implements terminal.Completer over the filesystem.
type fileCompleter struct{}

// Complete returns candidate suffixes for the token ending at cursor:
// cwd entries for a bare token, directory contents when the token carries
// a path prefix (~ expands to the home directory), and exit/quit for a
// first word.
func (fileCompleter) Complete(line string, cursor int) []string {
	if cursor > len(line) {
		cursor = len(line)
	}
	left := line[:cursor]
	start := strings.LastIndexAny(left, " \t\n") + 1
	token := left[start:]

	// First word: built-in commands (falling through to file completion
	// when none apply).
	if start == 0 && token != "" && !strings.Contains(token, "/") {
		var cands []string
		for _, cmd := range []string{"exit", "quit"} {
			if strings.HasPrefix(cmd, token) && cmd != token {
				cands = append(cands, cmd[len(token):])
			}
		}
		if len(cands) > 0 {
			return cands
		}
	}

	dir, prefix := ".", token
	if i := strings.LastIndex(token, "/"); i >= 0 {
		dir, prefix = token[:i], token[i+1:]
		if dir == "" {
			dir = "/"
		}
	} else if strings.HasPrefix(token, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		rest := token[1:]
		if i := strings.LastIndex(rest, "/"); i >= 0 {
			dir, prefix = filepath.Join(home, rest[:i]), rest[i+1:]
		} else {
			dir, prefix = home, rest
		}
	}

	var dirs, files []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			if e.IsDir() {
				dirs = append(dirs, name+"/")
			} else {
				files = append(files, name)
			}
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	cands := append(dirs, files...)
	if token == "" {
		cands = append([]string{"./", "../"}, cands...)
	}
	if len(cands) > completeCap {
		cands = cands[:completeCap]
	}
	return cands
}
