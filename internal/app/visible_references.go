package app

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxVisiblePaths           = 4
	maxVisibleURLs            = 4
	maxVisibleReferenceBytes  = 240
	maxGuideReferenceBytes    = 1800
	maxSnapshotReferenceBytes = 600
)

type visibleReferences struct {
	Paths []string
	URLs  []string
}

type textRange struct {
	start int
	end   int
}

func renderVisibleReferences(capture string) string {
	return renderReferences(extractVisibleReferences(capture, maxVisiblePaths, maxVisibleURLs), true, maxGuideReferenceBytes)
}

func renderSnapshotReferences(capture string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes > maxSnapshotReferenceBytes {
		maxBytes = maxSnapshotReferenceBytes
	}
	refs := extractVisibleReferences(capture, maxVisiblePaths, maxVisibleURLs)
	if len(refs.Paths) == 0 || len(refs.URLs) == 0 {
		return renderReferences(refs, false, maxBytes)
	}
	pathBudget := maxBytes * 55 / 100
	paths := renderReferences(visibleReferences{Paths: refs.Paths}, false, pathBudget)
	linkBudget := maxBytes - len(paths)
	if paths != "" {
		linkBudget -= 2
	}
	links := renderReferences(visibleReferences{URLs: refs.URLs}, false, linkBudget)
	if paths == "" {
		return links
	}
	if links == "" {
		return paths
	}
	return paths + "\n\n" + links
}

func renderReferences(refs visibleReferences, fencePaths bool, maxBytes int) string {
	if maxBytes <= 0 || len(refs.Paths) == 0 && len(refs.URLs) == 0 {
		return ""
	}
	var b strings.Builder
	appendItems := func(label string, items []string, fenced bool) {
		if len(items) == 0 {
			return
		}
		separator := ""
		if b.Len() > 0 {
			separator = "\n\n"
		}
		var section strings.Builder
		section.WriteString(label)
		section.WriteString(":")
		if fenced {
			section.WriteString("\n```")
		}
		added := 0
		for _, item := range items {
			suffixBytes := 0
			if fenced {
				suffixBytes = len("\n```")
			}
			if b.Len()+len(separator)+section.Len()+1+len(item)+suffixBytes > maxBytes {
				continue
			}
			section.WriteByte('\n')
			section.WriteString(item)
			added++
		}
		if added == 0 {
			return
		}
		if fenced {
			section.WriteString("\n```")
		}
		b.WriteString(separator)
		b.WriteString(section.String())
	}
	appendItems("paths", refs.Paths, fencePaths)
	appendItems("links", refs.URLs, false)
	return b.String()
}

func extractVisibleReferences(capture string, pathLimit, urlLimit int) visibleReferences {
	urls, ranges := scanVisibleURLs(capture, urlLimit)
	return visibleReferences{
		Paths: extractVisiblePathsOutsideRanges(capture, pathLimit, ranges),
		URLs:  urls,
	}
}

func extractVisiblePaths(capture string, limit int) []string {
	return extractVisibleReferences(capture, limit, 0).Paths
}

func extractVisibleURLs(capture string, limit int) []string {
	return extractVisibleReferences(capture, 0, limit).URLs
}

func extractVisiblePathsOutsideRanges(capture string, limit int, excluded []textRange) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]bool{}
	paths := make([]string, 0, limit)
	for i := 0; i < len(capture); i++ {
		if len(paths) >= limit {
			break
		}
		if inTextRanges(i, excluded) || !pathStart(capture, i) {
			continue
		}
		end := i + 1
		for end < len(capture) && pathChar(capture[end]) {
			end++
		}
		path := strings.TrimRight(capture[i:end], ".,;:)]}>\"'")
		if len(path) <= maxVisibleReferenceBytes && validVisiblePath(path) && visiblePathExists(path) && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
		i = end
	}
	return paths
}

func scanVisibleURLs(capture string, limit int) ([]string, []textRange) {
	seen := map[string]bool{}
	urls := make([]string, 0)
	ranges := make([]textRange, 0)
	lower := strings.ToLower(capture)
	for i := 0; i < len(capture); {
		httpAt := strings.Index(lower[i:], "http://")
		httpsAt := strings.Index(lower[i:], "https://")
		start := nearestIndex(i, httpAt, httpsAt)
		if start < 0 {
			break
		}
		if start > 0 && isURLPrefixChar(capture[start-1]) {
			i = start + 1
			continue
		}
		end := start
		for end < len(capture) && !isURLTerminator(capture[end]) {
			end++
		}
		trimmedEnd := end
		for trimmedEnd > start && strings.ContainsRune(".,;:!?)]}", rune(capture[trimmedEnd-1])) {
			trimmedEnd--
		}
		candidate := capture[start:trimmedEnd]
		if validVisibleURL(candidate) {
			ranges = append(ranges, textRange{start: start, end: end})
			if limit > 0 && len(urls) < limit && len(candidate) <= maxVisibleReferenceBytes && !seen[candidate] {
				seen[candidate] = true
				urls = append(urls, candidate)
			}
		}
		if end <= start {
			i = start + 1
		} else {
			i = end
		}
	}
	return urls, ranges
}

func nearestIndex(offset, left, right int) int {
	switch {
	case left < 0 && right < 0:
		return -1
	case left < 0:
		return offset + right
	case right < 0:
		return offset + left
	case left < right:
		return offset + left
	default:
		return offset + right
	}
}

func validVisibleURL(candidate string) bool {
	parsed, err := url.ParseRequestURI(candidate)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")
}

func isURLPrefixChar(ch byte) bool {
	return ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || strings.ContainsRune("+.-", rune(ch))
}

func isURLTerminator(ch byte) bool {
	return ch <= ' ' || strings.ContainsRune("<>\"'`", rune(ch))
}

func inTextRanges(index int, ranges []textRange) bool {
	for _, current := range ranges {
		if index >= current.start && index < current.end {
			return true
		}
	}
	return false
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
