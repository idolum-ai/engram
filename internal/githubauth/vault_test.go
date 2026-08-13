package githubauth

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func TestVaultApprovalOnlyUsesSeparateDeviceSealAndNoPassphrase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github-apps.json")
	vault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	item, created, err := vault.AddApprovalOnlyInstallations("idolum", 123, []int64{456}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !created || !item.ApprovalOnly || item.TelegramUnlock {
		t.Fatalf("approval-only enrollment = %#v", item)
	}
	vaultBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sealBytes, err := os.ReadFile(vault.DeviceSealPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(sealBytes) != deviceSealKeyBytes || bytes.Contains(vaultBytes, sealBytes) || bytes.Contains(vaultBytes, privateKey) {
		t.Fatal("vault contains device seal or plaintext private key")
	}
	for _, protected := range []string{path, vault.DeviceSealPath()} {
		info, err := os.Stat(protected)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filepath.Base(protected), info.Mode().Perm())
		}
	}
	if _, _, err := vault.Unlock("idolum", []byte("correct horse battery staple")); err != ErrUnlock {
		t.Fatalf("passphrase unlocked approval-only enrollment: %v", err)
	}
	unlocked, public, err := vault.UnlockApprovalOnly("idolum")
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(unlocked)
	if !bytes.Equal(unlocked, privateKey) || !public.ApprovalOnly {
		t.Fatal("device seal did not unlock approval-only enrollment")
	}
}

func TestVaultApprovalOnlyFailsClosedWhenDeviceSealOrModeChanges(t *testing.T) {
	vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	if _, _, err := vault.AddApprovalOnlyInstallations("idolum", 123, []int64{456}, privateKey); err != nil {
		t.Fatal(err)
	}
	replacement := bytes.Repeat([]byte{0x5a}, deviceSealKeyBytes)
	if err := os.WriteFile(vault.DeviceSealPath(), replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.UnlockApprovalOnly("idolum"); err != ErrUnlock {
		t.Fatalf("replacement device seal error = %v, want ErrUnlock", err)
	}
	if _, _, err := vault.AddApprovalOnlyInstallations("idolum", 123, []int64{456}, privateKey); err != nil {
		t.Fatal(err)
	}
	vault.mu.Lock()
	vault.state.Apps[0].ApprovalOnly = false
	vault.mu.Unlock()
	if _, _, err := vault.Unlock("idolum", replacement); err != ErrUnlock {
		t.Fatalf("unlock-mode tampering error = %v, want ErrUnlock", err)
	}
}

func TestVaultApprovalOnlyRejectsUnsafeDeviceSealMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{name: "group-readable", mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong-size", mutate: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard-linked", mutate: func(t *testing.T, path string) {
			if err := os.Link(path, path+".copy"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
			if err != nil {
				t.Fatal(err)
			}
			privateKey, _ := testPrivateKey(t)
			if _, _, err := vault.AddApprovalOnlyInstallations("idolum", 123, []int64{456}, privateKey); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, vault.DeviceSealPath())
			if _, _, err := vault.UnlockApprovalOnly("idolum"); err != ErrUnlock {
				t.Fatalf("unsafe device seal error = %v, want ErrUnlock", err)
			}
		})
	}
}

func TestVaultDoesNotRegenerateMissingSealBehindExistingApprovalOnlyEnrollment(t *testing.T) {
	vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := testPrivateKey(t)
	if _, _, err := vault.AddApprovalOnlyInstallations("first", 123, []int64{456}, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(vault.DeviceSealPath()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.AddApprovalOnlyInstallations("second", 123, []int64{789}, privateKey); err == nil || !strings.Contains(err.Error(), "approval-only enrollments remain") {
		t.Fatalf("missing shared seal re-enrollment error = %v", err)
	}
	if _, err := os.Stat(vault.DeviceSealPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing shared seal was silently regenerated: %v", err)
	}
}

func TestVaultDoesNotCreateDeviceSealForRejectedApprovalOnlyEnrollment(t *testing.T) {
	vault, err := OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.AddApprovalOnlyInstallations("Bad Alias", 123, []int64{456}, []byte("not a key")); err == nil {
		t.Fatal("invalid approval-only enrollment was accepted")
	}
	if _, err := os.Stat(vault.DeviceSealPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected enrollment created a device seal: %v", err)
	}
}

func TestVaultReadsVersionTwoInstallationSetWithoutRewriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github-apps.json")
	privateKey, key := testPrivateKey(t)
	passphrase := []byte("correct horse battery staple")
	installations := []int64{456, 789}
	encrypted, err := encryptPrivateKeyWithAAD(credentialInstallationSetAADV2("idolum", 123, installations), privateKey, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := PublicFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := persistedVault{Version: 2, Apps: []App{{
		Alias: "idolum", AppID: 123, InstallationID: 456, InstallationIDs: installations,
		CredentialIdentityVersion: installationSetIdentityVersion,
		TelegramUnlock:            true, PublicFingerprint: fingerprint, CreatedAt: time.Now().UTC(), PrivateKey: encrypted,
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
		t.Fatal("opening a version-2 vault rewrote encrypted credential material")
	}
	unlocked, public, err := vault.Unlock("idolum", passphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(unlocked)
	if !bytes.Equal(unlocked, privateKey) || len(public.Installations()) != 2 || !public.TelegramUnlock {
		t.Fatal("version-2 credential did not preserve identity or unlock mode")
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
