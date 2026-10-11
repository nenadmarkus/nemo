package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestFileCompleterSuffixes pins that candidates are continuations beyond
// the typed prefix, not full names: "sr" + Tab on {src/, readme.md} must
// offer "c/", never "src/".
func TestFileCompleterSuffixes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	c := fileCompleter{}
	tests := []struct {
		token string
		want  []string
	}{
		{"sr", []string{"c/"}},                           // dir suffix
		{"readme.m", []string{"d"}},                      // file suffix
		{"", []string{"./", "../", "src/", "readme.md"}}, // empty token: cwd listing
		{"ex", []string{"it"}},                           // first-word command
		{"z", nil},                                       // no match
		{filepath.ToSlash(dir) + "/sr", []string{"c/"}},  // explicit path prefix
	}
	for _, tt := range tests {
		if got := c.Complete(tt.token, len(tt.token)); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Complete(%q) = %q, want %q", tt.token, got, tt.want)
		}
	}
}
