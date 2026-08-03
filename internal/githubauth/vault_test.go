package githubauth

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestVaultEnrollsCanonicalInstallationSetAndRequiresExplicitSelection(t *testing.T) {
	vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	passphrase := []byte("correct horse battery staple")
	item, _, err := vault.AddInstallations("idolum", 123, []int64{789, 456, 789}, privateKey, passphrase, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := item.Installations(); len(got) != 2 || got[0] != 456 || got[1] != 789 {
		t.Fatalf("installations = %#v", got)
	}
	if _, err := item.SelectInstallation(0); err == nil || !strings.Contains(err.Error(), "pass --installation-id") {
		t.Fatalf("ambiguous selection error = %v", err)
	}
	selected, err := item.SelectInstallation(789)
	if err != nil || selected.EffectiveInstallationID() != 789 {
		t.Fatalf("selected enrollment = %#v, error = %v", selected, err)
	}
	if _, err := item.SelectInstallation(999); err == nil || !strings.Contains(err.Error(), "456, 789") {
		t.Fatalf("unenrolled selection error = %v", err)
	}

	vault.mu.Lock()
	vault.state.Apps[0].InstallationIDs = append(vault.state.Apps[0].InstallationIDs, 999)
	vault.mu.Unlock()
	if _, _, err := vault.Unlock("idolum", passphrase); err != ErrUnlock {
		t.Fatalf("tampered installation set unlock error = %v, want ErrUnlock", err)
	}
}

func TestVaultReadsVersionOneSingleInstallationWithoutRewriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github-apps.json")
	privateKey, key := testPrivateKey(t)
	passphrase := []byte("correct horse battery staple")
	encrypted, err := encryptPrivateKey("idolum", 123, 456, privateKey, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := PublicFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := persistedVault{Version: 1, Apps: []App{{
		Alias: "idolum", AppID: 123, InstallationID: 456,
		PublicFingerprint: fingerprint, CreatedAt: time.Now().UTC(), PrivateKey: encrypted,
	}}}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	vault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	afterOpen, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterOpen, data) {
		t.Fatal("opening a legacy vault rewrote encrypted credential material")
	}
	apps := vault.List()
	if len(apps) != 1 || len(apps[0].Installations()) != 1 || apps[0].Installations()[0] != 456 {
		t.Fatalf("legacy enrollment = %#v", apps)
	}
	unlocked, public, err := vault.Unlock("idolum", passphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(unlocked)
	if !bytes.Equal(unlocked, privateKey) || public.EffectiveInstallationID() != 456 {
		t.Fatal("legacy credential did not unlock with its original identity")
	}
	if _, _, err := vault.AddInstallations("second", 124, []int64{900, 901}, privateKey, passphrase, false); err != nil {
		t.Fatal(err)
	}
	legacyUnlocked, _, err := vault.Unlock("idolum", passphrase)
	if err != nil {
		t.Fatalf("legacy credential after version-2 vault mutation: %v", err)
	}
	Zero(legacyUnlocked)

	legacy.Apps[0].InstallationIDs = []int64{456, 789}
	tampered, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVault(path); err == nil || !strings.Contains(err.Error(), "cannot enroll additional installations") {
		t.Fatalf("legacy installation-set extension error = %v", err)
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
