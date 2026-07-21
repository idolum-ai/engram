package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorePersistsPrivateTemplates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "templates.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, wasCreated, err := store.Put("review-panel", "Send a careful review panel.", time.Unix(1, 0))
	if err != nil || !wasCreated || created.Name != "review-panel" {
		t.Fatalf("Put() template=%#v created=%v error=%v", created, wasCreated, err)
	}
	updated, wasCreated, err := store.Put("review-panel", "Send the complete review panel.", time.Unix(2, 0))
	if err != nil || wasCreated || updated.Body != "Send the complete review panel." {
		t.Fatalf("update template=%#v created=%v error=%v", updated, wasCreated, err)
	}
	if _, _, err := store.Put("fix-review", "Address valid findings.", time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	items := reopened.List()
	if len(items) != 2 || items[0].Name != "fix-review" || items[1].Name != "review-panel" || items[1].Body != updated.Body {
		t.Fatalf("List() = %#v", items)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("template state mode = %o, want 600", info.Mode().Perm())
	}

	removed, found, err := reopened.Forget("review-panel")
	if err != nil || !found || removed.Name != "review-panel" {
		t.Fatalf("Forget() template=%#v found=%v error=%v", removed, found, err)
	}
	if _, found := reopened.Get("review-panel"); found {
		t.Fatal("forgotten template remained available")
	}
}

func TestExpandIsExplicitBoundedAndNonRecursive(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("review-panel", "Review carefully, then {fix-review}.", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("fix-review", "Fix every valid finding", time.Time{}); err != nil {
		t.Fatal(err)
	}

	got, used, err := store.Expand("Please {review-panel}\nThen {fix-review}. Show {{review-panel}} literally.")
	if err != nil {
		t.Fatal(err)
	}
	want := "Please Review carefully, then {fix-review}.\nThen Fix every valid finding. Show {review-panel} literally."
	if got != want || strings.Join(used, ",") != "review-panel,fix-review" {
		t.Fatalf("Expand() = %q, %#v; want %q", got, used, want)
	}
	if _, _, err := store.Expand("Use {missing-template}."); err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("unknown template error = %v", err)
	}
	literal, used, err := store.Expand(`JSON {"name":"value"} and Go {err} remain literal.`)
	if err != nil || literal != `JSON {"name":"value"} and Go {err} remain literal.` || len(used) != 0 {
		t.Fatalf("literal expansion = %q, %#v, %v", literal, used, err)
	}
}

func TestExpandRejectsOversizedResult(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("large", strings.Repeat("x", MaxBodyBytes), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Expand(strings.Repeat("{large}", MaxExpandedBytes/MaxBodyBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized expansion error = %v", err)
	}
}

func TestStoreRejectsInvalidInputAndUnsafeFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"Bad Name", "body"},
		{"", "body"},
		{"valid", ""},
		{"valid", strings.Repeat("x", MaxBodyBytes+1)},
		{"valid", "contains\x00nul"},
	} {
		if _, _, err := store.Put(test.name, test.body, time.Time{}); err == nil {
			t.Errorf("Put(%q) accepted invalid input", test.name)
		}
	}

	realPath := filepath.Join(dir, "real.json")
	if err := os.WriteFile(realPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "linked.json")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkPath); err == nil {
		t.Fatal("Open() followed a symlink")
	}

	publicPath := filepath.Join(dir, "public.json")
	if err := os.WriteFile(publicPath, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(publicPath); err == nil {
		t.Fatal("Open() accepted public template state")
	}
}
