package storage

import "testing"

func TestCleanObjectPath(t *testing.T) {
	tests := map[string]string{
		"":                    "",
		"/docs/file.txt":      "docs/file.txt",
		"docs/../file.txt":    "file.txt",
		"a//b\\c.txt":         "a/b/c.txt",
		"../../escape.txt":    "escape.txt",
		"  /space/name.txt  ": "space/name.txt",
	}
	for input, want := range tests {
		if got := CleanObjectPath(input); got != want {
			t.Fatalf("CleanObjectPath(%q)=%q want %q", input, got, want)
		}
	}
}

func TestDirPrefix(t *testing.T) {
	if got := DirPrefix("/docs"); got != "docs/" {
		t.Fatalf("DirPrefix()=%q", got)
	}
	if got := DirPrefix(""); got != "" {
		t.Fatalf("DirPrefix empty=%q", got)
	}
}
