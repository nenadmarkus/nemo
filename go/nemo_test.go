package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContainsCwd pins the polarity of the delete guard: the working
// directory and its ancestors are dangerous (true), paths inside or
// outside the working tree are deletable (false).
func TestContainsCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dotAbs, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cwd)

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"cwd itself", cwd, true},
		{"cwd via dot", dotAbs, true},
		{"parent of cwd", parent, true},
		{"filesystem root", string(filepath.Separator), true},
		{"child of cwd", filepath.Join(cwd, "build"), false},
		{"nested child of cwd", filepath.Join(cwd, "a", "b"), false},
		{"sibling branch", filepath.Join(parent, "nemo-test-elsewhere"), false},
		{"other subtree", filepath.Join(os.TempDir(), "nemo-test"), false},
	}
	for _, tt := range tests {
		if got := containsCwd(tt.dir); got != tt.want {
			t.Errorf("%s: containsCwd(%q) = %v, want %v", tt.name, tt.dir, got, tt.want)
		}
	}
}
