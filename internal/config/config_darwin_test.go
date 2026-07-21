//go:build darwin

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDirsAcceptsDarwinVarFoldersTempAlias(t *testing.T) {
	base := t.TempDir()
	if !strings.HasPrefix(filepath.Clean(base), "/var/folders/") {
		t.Skipf("temporary directory %q does not use the standard /var/folders alias", base)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")

	home := filepath.Join(base, "home")
	if err := EnsureDirs(Config{Home: home}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("ENGRAM_HOME metadata = %v", info.Mode())
	}
}
