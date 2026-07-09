package app

import (
	"os"
	"path/filepath"
	"strings"
)

const maxVisiblePaths = 12

func renderVisiblePaths(capture string) string {
	paths := extractVisiblePaths(capture, maxVisiblePaths)
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("visible paths:\n```")
	b.WriteByte('\n')
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte('\n')
	}
	b.WriteString("```")
	return b.String()
}

func extractVisiblePaths(capture string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]bool{}
	paths := make([]string, 0, limit)
	for i := 0; i < len(capture); i++ {
		if len(paths) >= limit {
			break
		}
		if !pathStart(capture, i) {
			continue
		}
		end := i + 1
		for end < len(capture) && pathChar(capture[end]) {
			end++
		}
		path := strings.TrimRight(capture[i:end], ".,;:)]}>\"'")
		if validVisiblePath(path) && visiblePathExists(path) && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
		i = end
	}
	return paths
}

func pathStart(s string, i int) bool {
	if s[i] == '~' {
		return i+1 < len(s) && s[i+1] == '/'
	}
	if s[i] != '/' {
		return false
	}
	if i+1 < len(s) && s[i+1] == '/' {
		return false
	}
	if i > 0 && s[i-1] == '/' {
		return false
	}
	if i > 0 && s[i-1] == ':' {
		return false
	}
	return true
}

func validVisiblePath(path string) bool {
	if path == "/" || path == "~/" {
		return false
	}
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~/")
}

func visiblePathExists(path string) bool {
	expanded := path
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return false
		}
		expanded = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() || info.IsDir()
}

func pathChar(ch byte) bool {
	switch ch {
	case '/', '.', '_', '-', '+', '@', '%', '=', ':':
		return true
	}
	return ch >= '0' && ch <= '9' ||
		ch >= 'A' && ch <= 'Z' ||
		ch >= 'a' && ch <= 'z'
}
