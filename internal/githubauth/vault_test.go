package githubauth

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultEncryptsPrivateKeyAndUnlocksOnlyWithPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-apps.json")
	vault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	passphrase := []byte("correct horse battery staple")
	item, created, err := vault.Add("idolum", 123, 456, privateKey, passphrase, true)
	if err != nil {
		t.Fatal(err)
	}
	if !created || item.Alias != "idolum" || !item.TelegramUnlock || item.PublicFingerprint == "" {
		t.Fatalf("unexpected enrolled app: %#v", item)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, privateKey) || bytes.Contains(stored, passphrase) || strings.Contains(string(stored), "RSA PRIVATE KEY") {
		t.Fatal("vault persisted plaintext secret material")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode = %o, want 600", info.Mode().Perm())
	}
	unlocked, public, err := vault.Unlock("idolum", passphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(unlocked)
	if !bytes.Equal(unlocked, privateKey) || public.PrivateKey.Ciphertext != "" {
		t.Fatal("unlocked private key or public metadata did not match")
	}
	if _, _, err := vault.Unlock("idolum", []byte("definitely wrong passphrase")); err != ErrUnlock {
		t.Fatalf("wrong passphrase error = %v, want ErrUnlock", err)
	}
}

func TestVaultBindsCiphertextToAppIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-apps.json")
	vault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	passphrase := []byte("a sufficiently long password")
	if _, _, err := vault.Add("first", 123, 456, privateKey, passphrase, false); err != nil {
		t.Fatal(err)
	}
	vault.mu.Lock()
	vault.state.Apps[0].Alias = "second"
	vault.mu.Unlock()
	if _, _, err := vault.Unlock("second", passphrase); err != ErrUnlock {
		t.Fatalf("identity-tampered ciphertext error = %v, want ErrUnlock", err)
	}
}

func TestVaultRejectsWeakOrMalformedEnrollment(t *testing.T) {
	vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	if _, _, err := vault.Add("Bad Alias", 1, 2, privateKey, []byte("long enough password"), false); err == nil {
		t.Fatal("invalid alias was accepted")
	}
	if _, _, err := vault.Add("valid", 1, 2, privateKey, []byte("short"), false); err == nil {
		t.Fatal("weak passphrase was accepted")
	}
	if _, _, err := vault.Add("valid", 1, 2, []byte("not a key"), []byte("long enough password"), false); err == nil {
		t.Fatal("malformed private key was accepted")
	}
}

func TestVaultReloadObservesEnrollmentFromAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-apps.json")
	serviceVault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	cliVault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	if _, _, err := cliVault.Add("idolum", 1, 2, privateKey, []byte("long enough password"), false); err != nil {
		t.Fatal(err)
	}
	if _, found := serviceVault.Get("idolum"); found {
		t.Fatal("service vault changed without an explicit reload")
	}
	if err := serviceVault.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, found := serviceVault.Get("idolum"); !found {
		t.Fatal("service vault did not observe external enrollment")
	}
}

func TestVaultRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if _, err := OpenVault(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "github-apps.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVault(link); err == nil {
		t.Fatal("vault followed a symlink")
	}
}

func TestPBKDF2SHA256KnownVector(t *testing.T) {
	got := derivePBKDF2SHA256([]byte("password"), []byte("salt"), 2, 32)
	defer Zero(got)
	want, err := hex.DecodeString("ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("PBKDF2 vector = %x, want %x", got, want)
	}
}
