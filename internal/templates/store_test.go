package templates

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/atomicfile"
)

func TestStorePersistsPrivateTemplates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "templates.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, wasCreated, err := store.Put("review-panel", "Send a careful review panel.")
	if err != nil || !wasCreated || created.Name != "review-panel" {
		t.Fatalf("Put() template=%#v created=%v error=%v", created, wasCreated, err)
	}
	updated, wasCreated, err := store.Put("review-panel", "Send the complete review panel.")
	if err != nil || wasCreated || updated.Body != "Send the complete review panel." {
		t.Fatalf("update template=%#v created=%v error=%v", updated, wasCreated, err)
	}
	if _, _, err := store.Put("fix-review", "Address valid findings."); err != nil {
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
	if _, _, err := store.Put("review-panel", "Review carefully, then {engram:fix-review}."); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("fix-review", "Fix every valid finding"); err != nil {
		t.Fatal(err)
	}

	got, used, err := store.Expand("Please {engram:review-panel}\nThen {engram:fix-review}.")
	if err != nil {
		t.Fatal(err)
	}
	want := "Please Review carefully, then {engram:fix-review}.\nThen Fix every valid finding."
	if got != want || strings.Join(used, ",") != "review-panel,fix-review" {
		t.Fatalf("Expand() = %q, %#v; want %q", got, used, want)
	}
	if _, _, err := store.Expand("Use {engram:missing-template}."); err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("unknown template error = %v", err)
	}
	literalInput := `JSON {"name":"value"}, Go {err}, shell ${engram:review-panel}, Helm {{engram:review-panel}}, and GitHub ${{ engram:review-panel }} remain literal.`
	literal, used, err := store.Expand(literalInput)
	if err != nil || literal != literalInput || len(used) != 0 {
		t.Fatalf("literal expansion = %q, %#v, %v", literal, used, err)
	}
}

func TestExpandRejectsOversizedResult(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("large", strings.Repeat("x", MaxBodyBytes)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Expand(strings.Repeat("{engram:large}", MaxExpandedBytes/MaxBodyBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
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
		if _, _, err := store.Put(test.name, test.body); err == nil || !IsValidationError(err) {
			t.Errorf("Put(%q) accepted invalid input", test.name)
		}
	}

	exact := "  indented\nbody with trailing space \n"
	if _, _, err := store.Put("exact", exact); err != nil {
		t.Fatal(err)
	}
	item, found := store.Get("exact")
	if !found || item.Body != exact {
		t.Fatalf("exact body = %q, found=%v", item.Body, found)
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

func TestOpenPreservesCorruptionAndInitializesReplacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "malformed", data: []byte(`{"version":`)},
		{name: "invalid template", data: []byte(`{"version":1,"templates":[{"name":"Bad Name","body":"x"}]}`)},
		{name: "oversized", data: bytes.Repeat([]byte("x"), maxStateBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "templates.json")
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if store.RecoveredPath() == "" {
				t.Fatal("corruption was not preserved")
			}
			backup, err := os.ReadFile(store.RecoveredPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(backup, test.data) {
				t.Fatal("corrupt backup changed")
			}
			reopened, err := Open(path)
			if err != nil || len(reopened.List()) != 0 {
				t.Fatalf("replacement store=%#v error=%v", reopened, err)
			}
		})
	}
}

func TestOpenLeavesFutureSchemaUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	data := []byte(`{"version":2,"templates":{"future":"shape"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future schema error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("future schema was modified")
	}
	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 0 {
		t.Fatalf("future schema was quarantined: %#v", matches)
	}
}

func TestMutationTracksAtomicReplacementBoundary(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Put("existing", "old"); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("injected persistence failure")
	store.writeFile = func(string, []byte) error {
		return &atomicfile.WriteError{Err: cause}
	}
	if _, _, err := store.Put("before-rename", "body"); !errors.Is(err, cause) || PersistenceReachedReplacement(err) {
		t.Fatalf("pre-replacement Put error = %v", err)
	}
	if _, found := store.Get("before-rename"); found {
		t.Fatal("pre-replacement failure changed in-memory state")
	}

	store.writeFile = func(string, []byte) error {
		return &atomicfile.WriteError{Err: cause, Replaced: true}
	}
	if _, _, err := store.Put("after-rename", "body"); !errors.Is(err, cause) || !PersistenceReachedReplacement(err) {
		t.Fatalf("post-replacement Put error = %v", err)
	}
	if item, found := store.Get("after-rename"); !found || item.Body != "body" {
		t.Fatalf("post-replacement state = %#v, found=%v", item, found)
	}
	if _, found, err := store.Forget("existing"); !found || !errors.Is(err, cause) || !PersistenceReachedReplacement(err) {
		t.Fatalf("post-replacement Forget found=%v error=%v", found, err)
	}
	if _, found := store.Get("existing"); found {
		t.Fatal("post-replacement Forget rolled back in-memory state")
	}
}

func TestOpenRediscoversBackupAfterReplacementFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	corrupt := []byte(`{"version":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected replacement failure")
	store := &Store{
		path:      path,
		state:     persistedState{Version: currentVersion},
		writeFile: func(string, []byte) error { return &atomicfile.WriteError{Err: cause} },
	}
	if _, err := recoverCorrupt(store, errors.New("corrupt")); !errors.Is(err, cause) {
		t.Fatalf("recovery error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canonical template state exists after injected failure: %v", err)
	}
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("recovery backups = %#v, error=%v", matches, err)
	}
	backup, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Equal(backup, corrupt) {
		t.Fatalf("backup=%q error=%v", backup, err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.RecoveredPath() != matches[0] {
		t.Fatalf("recovered path = %q, want %q", reopened.RecoveredPath(), matches[0])
	}
}

func TestRecoveryContinuesAfterReplacementWithDurabilityWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	corrupt := []byte(`{"version":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected directory sync failure")
	store := &Store{
		path:  path,
		state: persistedState{Version: currentVersion},
		writeFile: func(path string, data []byte) error {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
			return &atomicfile.WriteError{Err: cause, Replaced: true}
		},
	}
	recovered, err := recoverCorrupt(store, errors.New("corrupt"))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RecoveredPath() == "" || !errors.Is(recovered.RecoveryWarning(), cause) {
		t.Fatalf("recovered path=%q warning=%v", recovered.RecoveredPath(), recovered.RecoveryWarning())
	}
	if _, err := os.Stat(recovered.RecoveredPath()); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if reopened, err := Open(path); err != nil || len(reopened.List()) != 0 {
		t.Fatalf("visible replacement store=%#v error=%v", reopened, err)
	}
}
