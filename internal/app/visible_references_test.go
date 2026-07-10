package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtractVisibleURLsValidatesTrimsAndDeduplicates(t *testing.T) {
	t.Parallel()
	capture := strings.Join([]string{
		"docs: https://example.com/guide?q=tmux&mode=phone.",
		"again (https://example.com/guide?q=tmux&mode=phone)",
		"upper HTTPS://EXAMPLE.ORG/status",
		"reject ftp://example.com/file",
		"reject abchttps://example.net/hidden",
		"reject https:///missing-host",
	}, "\n")
	want := []string{
		"https://example.com/guide?q=tmux&mode=phone",
		"HTTPS://EXAMPLE.ORG/status",
	}
	if got := extractVisibleURLs(capture, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want %#v", got, want)
	}
}

func TestURLRangesCannotBecomeVisiblePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "artifact.pdf")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := "report: https://downloads.example.test" + path
	if got := extractVisiblePaths(capture, 4); len(got) != 0 {
		t.Fatalf("URL path became local path: %#v", got)
	}
	if got := extractVisibleURLs(capture, 4); len(got) != 1 || got[0] != "https://downloads.example.test"+path {
		t.Fatalf("URL extraction = %#v", got)
	}
}

func TestSnapshotReferencesAreBoundedAndUnfenced(t *testing.T) {
	t.Parallel()
	refs := visibleReferences{
		Paths: []string{"/tmp/report.pdf"},
		URLs:  []string{"https://example.com/report"},
	}
	got := renderReferences(refs, false, 100)
	if strings.Contains(got, "```") || !strings.Contains(got, "paths:\n/tmp/report.pdf") || !strings.Contains(got, "links:\nhttps://example.com/report") {
		t.Fatalf("snapshot references = %q", got)
	}
	if len(got) > 100 {
		t.Fatalf("snapshot references exceeded budget: %d", len(got))
	}
}

func TestSnapshotReferencesReserveRoomForLinks(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, strings.Repeat("p", 140)),
		filepath.Join(root, strings.Repeat("q", 140)),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capture := strings.Join(append(paths, "https://example.com/important"), "\n")
	got := renderSnapshotReferences(capture, maxSnapshotReferenceBytes)
	if len(got) > maxSnapshotReferenceBytes || !strings.Contains(got, "paths:\n") || !strings.Contains(got, "links:\nhttps://example.com/important") {
		t.Fatalf("snapshot reference allocation = %q", got)
	}
}
