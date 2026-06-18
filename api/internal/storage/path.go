package storage

import (
	"path"
	"strings"
)

func CleanObjectPath(input string) string {
	input = strings.ReplaceAll(input, "\\", "/")
	input = strings.TrimSpace(input)
	input = strings.TrimPrefix(input, "/")
	cleaned := path.Clean("/" + input)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func DirPrefix(input string) string {
	cleaned := CleanObjectPath(input)
	if cleaned == "" {
		return ""
	}
	return strings.TrimSuffix(cleaned, "/") + "/"
}

func JoinObjectPath(dir, name string) string {
	base := DirPrefix(dir)
	return CleanObjectPath(base + path.Base(name))
}
