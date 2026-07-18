package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/idolum-ai/engram/internal/redact"
	"github.com/idolum-ai/engram/internal/state"
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

func bindAnchorFiles(ts state.TerminalSession, files []string) state.TerminalSession {
	ts.AnchorFiles = append([]string(nil), files...)
	ts.AnchorFileToken = anchorFileToken(files)
	return ts
}

func setAnchorFiles(ts *state.TerminalSession, files []string) {
	ts.AnchorFiles = append([]string(nil), files...)
	ts.AnchorFileToken = anchorFileToken(files)
}

func anchorFileToken(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return sha(strings.Join(files, "\x00"))[:16]
}

func renderVisibleReferences(capture string, secrets ...string) string {
	refs := visibleReferencesForCapture(capture, secrets...)
	return renderReferences(refs, true, maxGuideReferenceBytes)
}

func renderSnapshotReferenceSetWithFiles(refs visibleReferences, maxBytes int) (string, []string) {
	if maxBytes <= 0 {
		return "", nil
	}
	if maxBytes > maxSnapshotReferenceBytes {
		maxBytes = maxSnapshotReferenceBytes
	}
	if len(refs.Paths) == 0 || len(refs.URLs) == 0 {
		return renderReferencesWithFiles(refs, true, maxBytes)
	}
	pathBudget := maxBytes * 55 / 100
	paths, files := renderReferencesWithFiles(visibleReferences{Paths: refs.Paths}, true, pathBudget)
	linkBudget := maxBytes - len(paths)
	if paths != "" {
		linkBudget -= 2
	}
	links := renderReferences(visibleReferences{URLs: refs.URLs}, false, linkBudget)
	if paths == "" {
		return links, nil
	}
	if links == "" {
		return paths, files
	}
	return paths + "\n\n" + links, files
}

func visibleReferencesForCapture(capture string, secrets ...string) visibleReferences {
	refs := extractVisibleReferences(capture, maxVisiblePaths, maxVisibleURLs, secrets...)
	paths := refs.Paths[:0]
	for _, path := range refs.Paths {
		// A redacted path cannot safely back an exact-file download button.
		if redact.Secrets(path, secrets...) == path {
			paths = append(paths, path)
		}
	}
	refs.Paths = paths
	return refs
}

func visibleReferencesForStyledCapture(capture string, hyperlinks []string, secrets ...string) visibleReferences {
	targets := append([]string(nil), hyperlinks...)
	targets = append(targets, extractVisibleFileURIs(capture, maxVisiblePaths)...)
	explicit := visibleReferencesForHyperlinks(targets, maxVisiblePaths, maxVisibleURLs, secrets...)
	detected := visibleReferencesForCapture(capture, secrets...)
	return visibleReferences{
		Paths: mergeReferences(explicit.Paths, detected.Paths, maxVisiblePaths),
		URLs:  mergeReferences(explicit.URLs, detected.URLs, maxVisibleURLs),
	}
}

func extractVisibleFileURIs(capture string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	lower := strings.ToLower(capture)
	seen := make(map[string]bool)
	var targets []string
	for offset := 0; offset < len(capture) && len(targets) < limit; {
		at := strings.Index(lower[offset:], "file://")
		if at < 0 {
			break
		}
		start := offset + at
		if start > 0 && isURLPrefixChar(capture[start-1]) {
			offset = start + 1
			continue
		}
		end := start
		for end < len(capture) && !isURLTerminator(capture[end]) {
			end++
		}
		candidate := trimUnmatchedURLClosers(capture[start:end])
		parsed, err := url.Parse(candidate)
		if err == nil && strings.EqualFold(parsed.Scheme, "file") && len(candidate) <= maxVisibleReferenceBytes && !seen[candidate] {
			seen[candidate] = true
			targets = append(targets, candidate)
		}
		offset = max(end, start+1)
	}
	return targets
}

func visibleReferencesForHyperlinks(hyperlinks []string, pathLimit, urlLimit int, secrets ...string) visibleReferences {
	refs := visibleReferences{}
	seenPaths := make(map[string]bool)
	seenURLs := make(map[string]bool)
	for _, target := range hyperlinks {
		parsed, err := url.Parse(target)
		if err != nil {
			continue
		}
		switch {
		case strings.EqualFold(parsed.Scheme, "file") && len(refs.Paths) < pathLimit:
			path, ok := visibleFileURIPath(parsed)
			if !ok || len(path) > maxVisibleReferenceBytes || redact.Secrets(path, secrets...) != path || seenPaths[path] {
				continue
			}
			seenPaths[path] = true
			refs.Paths = append(refs.Paths, path)
		case (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) && len(refs.URLs) < urlLimit:
			safe, ok := sanitizeVisibleURL(target, secrets...)
			if !ok || len(safe) > maxVisibleReferenceBytes || seenURLs[safe] {
				continue
			}
			seenURLs[safe] = true
			refs.URLs = append(refs.URLs, safe)
		}
	}
	return refs
}

func visibleFileURIPath(parsed *url.URL) (string, bool) {
	if parsed == nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", false
	}
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return "", false
	}
	if parsed.Path == "" || !filepath.IsAbs(parsed.Path) || strings.ContainsAny(parsed.Path, "\x00\r\n") {
		return "", false
	}
	return visibleRegularFile(parsed.Path)
}

func mergeReferences(preferred, fallback []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := make(map[string]bool)
	merged := make([]string, 0, min(limit, len(preferred)+len(fallback)))
	for _, group := range [][]string{preferred, fallback} {
		for _, item := range group {
			if len(merged) >= limit {
				return merged
			}
			if !seen[item] {
				seen[item] = true
				merged = append(merged, item)
			}
		}
	}
	return merged
}

func renderReferences(refs visibleReferences, fencePaths bool, maxBytes int) string {
	rendered, _ := renderReferencesWithFiles(refs, fencePaths, maxBytes)
	return rendered
}

func renderReferencesWithFiles(refs visibleReferences, fencePaths bool, maxBytes int) (string, []string) {
	if maxBytes <= 0 || len(refs.Paths) == 0 && len(refs.URLs) == 0 {
		return "", nil
	}
	var b strings.Builder
	var includedFiles []string
	appendItems := func(label string, items []string, fenced, numbered bool) {
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
			renderedItem := item
			if numbered {
				renderedItem = fmt.Sprintf("%d. %s", added+1, item)
			}
			suffixBytes := 0
			if fenced {
				suffixBytes = len("\n```")
			}
			if b.Len()+len(separator)+section.Len()+1+len(renderedItem)+suffixBytes > maxBytes {
				continue
			}
			section.WriteByte('\n')
			section.WriteString(renderedItem)
			added++
			if numbered {
				includedFiles = append(includedFiles, item)
			}
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
	appendItems("files", refs.Paths, fencePaths, true)
	appendItems("links", refs.URLs, false, false)
	return b.String(), includedFiles
}

func extractVisibleReferences(capture string, pathLimit, urlLimit int, secrets ...string) visibleReferences {
	urls, ranges := scanVisibleURLs(capture, urlLimit, secrets...)
	return visibleReferences{
		Paths: extractVisiblePathsOutsideRanges(capture, pathLimit, ranges),
		URLs:  urls,
	}
}

func extractVisiblePaths(capture string, limit int) []string {
	return extractVisibleReferences(capture, limit, 0).Paths
}

func extractVisibleURLs(capture string, limit int, secrets ...string) []string {
	return extractVisibleReferences(capture, 0, limit, secrets...).URLs
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
		if len(path) > maxVisibleReferenceBytes || !validVisiblePath(path) {
			i = end
			continue
		}
		if filePath, ok := visibleRegularFile(path); ok && !seen[filePath] {
			seen[filePath] = true
			paths = append(paths, filePath)
		}
		i = end
	}
	return paths
}

func scanVisibleURLs(capture string, limit int, secrets ...string) ([]string, []textRange) {
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
		candidate := trimUnmatchedURLClosers(capture[start:end])
		if validVisibleURL(candidate) {
			ranges = append(ranges, textRange{start: start, end: end})
			safeURL, safe := sanitizeVisibleURL(candidate, secrets...)
			if safe && limit > 0 && len(urls) < limit && len(safeURL) <= maxVisibleReferenceBytes && !seen[safeURL] {
				seen[safeURL] = true
				urls = append(urls, safeURL)
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
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")
}

func sanitizeVisibleURL(candidate string, secrets ...string) (string, bool) {
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", false
	}
	if redact.URLSafeComponentSecrets(parsed.Host, secrets...) != parsed.Host {
		return "", false
	}
	changed := false
	if safePath, safeRawPath, pathChanged, safe := redactEscapedURLPath(parsed.EscapedPath(), secrets...); !safe {
		return "", false
	} else if pathChanged {
		parsed.Path = safePath
		parsed.RawPath = safeRawPath
		changed = true
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}
	queryChanged := false
	for key, values := range query {
		if redact.URLSafeComponentSecrets(key, secrets...) != key {
			return "", false
		}
		if sensitiveURLParameter(key) {
			query.Set(key, "REDACTED")
			queryChanged = true
			continue
		}
		for index, value := range values {
			if safeValue := redact.URLSafeComponentSecrets(value, secrets...); safeValue != value {
				values[index] = safeValue
				queryChanged = true
			}
		}
	}
	if queryChanged {
		parsed.RawQuery = query.Encode()
		changed = true
	}
	if safeFragment, fragmentChanged, safe := sanitizeURLFragment(parsed.EscapedFragment(), secrets...); !safe {
		return "", false
	} else if fragmentChanged {
		decodedFragment, err := url.PathUnescape(safeFragment)
		if err != nil {
			return "", false
		}
		parsed.Fragment = decodedFragment
		parsed.RawFragment = safeFragment
		changed = true
	}
	if !changed {
		return candidate, true
	}
	return parsed.String(), true
}

func redactEscapedURLPath(escapedPath string, secrets ...string) (string, string, bool, bool) {
	segments := strings.Split(escapedPath, "/")
	changed := false
	for index, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return "", "", false, false
		}
		if safe := redact.URLSafeComponentSecrets(decoded, secrets...); safe != decoded {
			segments[index] = url.PathEscape(safe)
			changed = true
		}
	}
	if !changed {
		return "", "", false, true
	}
	safeRawPath := strings.Join(segments, "/")
	safePath, err := url.PathUnescape(safeRawPath)
	if err != nil {
		return "", "", false, false
	}
	return safePath, safeRawPath, true, true
}

func sanitizeURLFragment(fragment string, secrets ...string) (string, bool, bool) {
	prefix := ""
	prefixChanged := false
	routed := false
	valuesText := fragment
	if queryAt := strings.IndexByte(fragment, '?'); queryAt >= 0 {
		routed = true
		originalPrefix := fragment[:queryAt]
		_, safePrefix, prefixChangedNow, safe := redactEscapedURLPath(originalPrefix, secrets...)
		if !safe {
			return "", false, false
		}
		if !prefixChangedNow {
			safePrefix = originalPrefix
		}
		prefix = safePrefix + "?"
		prefixChanged = prefixChangedNow
		valuesText = fragment[queryAt+1:]
	} else if strings.HasPrefix(fragment, "/") || !strings.Contains(fragment, "=") {
		_, safeFragment, fragmentChanged, safe := redactEscapedURLPath(fragment, secrets...)
		if !safe {
			return "", false, false
		}
		if !fragmentChanged {
			return fragment, false, true
		}
		return safeFragment, true, true
	}
	values, err := url.ParseQuery(valuesText)
	if err != nil {
		if !routed {
			return sanitizeOpaqueURLFragment(fragment, secrets...)
		}
		return "", false, false
	}
	changed := prefixChanged
	for key, current := range values {
		if redact.URLSafeComponentSecrets(key, secrets...) != key {
			return "", false, false
		}
		if sensitiveURLParameter(key) {
			values.Set(key, "REDACTED")
			changed = true
			continue
		}
		for index, value := range current {
			if safeValue := redact.URLSafeComponentSecrets(value, secrets...); safeValue != value {
				current[index] = safeValue
				changed = true
			}
		}
	}
	if !changed {
		return fragment, false, true
	}
	return prefix + values.Encode(), true, true
}

func sanitizeOpaqueURLFragment(fragment string, secrets ...string) (string, bool, bool) {
	var b strings.Builder
	changed := false
	for rest := fragment; ; {
		separatorAt := strings.IndexAny(rest, "&;")
		part := rest
		separator := byte(0)
		if separatorAt >= 0 {
			part = rest[:separatorAt]
			separator = rest[separatorAt]
		}
		safePart, partChanged, safe := sanitizeOpaqueURLFragmentPart(part, secrets...)
		if !safe {
			return "", false, false
		}
		b.WriteString(safePart)
		changed = changed || partChanged
		if separatorAt < 0 {
			break
		}
		b.WriteByte(separator)
		rest = rest[separatorAt+1:]
	}
	if !changed {
		return fragment, false, true
	}
	return b.String(), true, true
}

func sanitizeOpaqueURLFragmentPart(part string, secrets ...string) (string, bool, bool) {
	equalsAt := strings.IndexByte(part, '=')
	if equalsAt < 0 {
		safe := redact.URLSafeComponentSecrets(part, secrets...)
		return safe, safe != part, true
	}
	rawKey, rawValue := part[:equalsAt], part[equalsAt+1:]
	key, err := url.QueryUnescape(rawKey)
	if err != nil || redact.URLSafeComponentSecrets(key, secrets...) != key {
		return "", false, false
	}
	value, err := url.QueryUnescape(rawValue)
	if err != nil {
		return "", false, false
	}
	safeValue := redact.URLSafeComponentSecrets(value, secrets...)
	if sensitiveURLParameter(key) {
		safeValue = "REDACTED"
	}
	if safeValue == value {
		return part, false, true
	}
	return rawKey + "=" + url.QueryEscape(safeValue), true, true
}

func trimUnmatchedURLClosers(candidate string) string {
	counts := map[byte]int{}
	for index := 0; index < len(candidate); index++ {
		switch candidate[index] {
		case '(', '[', '{':
			counts[candidate[index]]++
		case ')':
			counts['(']--
		case ']':
			counts['[']--
		case '}':
			counts['{']--
		}
	}
	suffixAt := len(candidate)
	for suffixAt > 0 && strings.ContainsRune(".,;:!?", rune(candidate[suffixAt-1])) {
		suffixAt--
	}
	prefix, suffix := candidate[:suffixAt], candidate[suffixAt:]
	for prefix != "" {
		last := prefix[len(prefix)-1]
		switch last {
		case ')':
			if counts['('] >= 0 {
				return prefix + suffix
			}
			counts['(']++
			prefix = prefix[:len(prefix)-1]
		case ']':
			if counts['['] >= 0 {
				return prefix + suffix
			}
			counts['[']++
			prefix = prefix[:len(prefix)-1]
		case '}':
			if counts['{'] >= 0 {
				return prefix + suffix
			}
			counts['{']++
			prefix = prefix[:len(prefix)-1]
		default:
			return prefix + suffix
		}
	}
	return suffix
}

func sensitiveURLParameter(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch normalized {
	case "api_key", "apikey", "auth", "authorization", "client_secret", "credential", "id_token", "key", "password", "passwd", "refresh_token", "secret", "sig", "signature", "token", "access_token", "x_amz_credential", "x_amz_signature", "x_amz_security_token":
		return true
	}
	for _, marker := range []string{"api_key", "apikey", "password", "secret", "token"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
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

func visibleRegularFile(path string) (string, bool) {
	expanded := path
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		expanded = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	expanded = filepath.Clean(expanded)
	if !filepath.IsAbs(expanded) {
		return "", false
	}
	info, err := os.Lstat(expanded)
	if err != nil {
		return "", false
	}
	return expanded, info.Mode().IsRegular()
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
