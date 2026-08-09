package githubauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePrivateKeyRejectsNonRSAPKCS8(t *testing.T) {
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBigInt(ecdsaKey.D)
	_, ed25519Key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(ed25519Key)

	for name, privateKey := range map[string]any{
		"ecdsa":   ecdsaKey,
		"ed25519": ed25519Key,
	} {
		t.Run(name, func(t *testing.T) {
			der, err := x509.MarshalPKCS8PrivateKey(privateKey)
			if err != nil {
				t.Fatal(err)
			}
			defer Zero(der)
			encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			defer Zero(encoded)
			if _, err := ParsePrivateKey(encoded); err == nil || !strings.Contains(err.Error(), "must be RSA") {
				t.Fatalf("ParsePrivateKey error = %v", err)
			}
		})
	}
}

func TestReadPrivateKeyFileValidatesPEMAndStableIdentity(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, privateKey, 0o600); err != nil {
		t.Fatal(err)
	}

	first, firstIdentity, err := ReadPrivateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(first)
	second, secondIdentity, err := ReadPrivateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(second)
	if !bytes.Equal(first, privateKey) || firstIdentity.Fingerprint == "" || !firstIdentity.Equal(secondIdentity) {
		t.Fatalf("validated identity mismatch: %#v %#v", firstIdentity, secondIdentity)
	}
	if firstIdentity.uid != uint32(os.Geteuid()) || firstIdentity.gid != uint32(os.Getegid()) ||
		firstIdentity.mode.Perm() != 0o600 || firstIdentity.linkCount != 1 {
		t.Fatalf("validated identity omitted file metadata: %#v", firstIdentity)
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.pem")
	if err := os.WriteFile(replacement, privateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	replaced, replacedIdentity, err := ReadPrivateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(replaced)
	if firstIdentity.Equal(replacedIdentity) || firstIdentity.Fingerprint != replacedIdentity.Fingerprint {
		t.Fatal("same-key file replacement did not change only the bound file identity")
	}
}

func TestReadPrivateKeyFileTracksOwnerOnlyMode(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, privateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	data, writableIdentity, err := ReadPrivateKeyFile(path)
	Zero(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	data, readOnlyIdentity, err := ReadPrivateKeyFile(path)
	Zero(data)
	if err != nil {
		t.Fatalf("owner-only mode 0400 was rejected: %v", err)
	}
	if writableIdentity.Equal(readOnlyIdentity) {
		t.Fatal("permission change did not change the file identity")
	}
}

func TestReadPrivateKeyFileRejectsMetadataAndPathRaces(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "chmod", mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard-link-count", mutate: func(t *testing.T, path string) {
			if err := os.Link(path, path+".link"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "atomic-pathname-replacement", mutate: func(t *testing.T, path string) {
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, privateKey, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(path, privateKey, 0o600); err != nil {
				t.Fatal(err)
			}
			data, _, err := readPrivateKeyFile(path, func() { test.mutate(t, path) })
			Zero(data)
			if err == nil || !strings.Contains(err.Error(), "pathname changed") {
				t.Fatalf("readPrivateKeyFile error = %v", err)
			}
		})
	}
}

func TestReadPrivateKeyFileDetectsRestoredModeThroughChangeTime(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, privateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := privateKeyFileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.changeTimeOK {
		t.Skip("OS does not expose file change time")
	}
	data, _, err := readPrivateKeyFile(path, func() {
		time.Sleep(5 * time.Millisecond)
		if chmodErr := os.Chmod(path, 0o400); chmodErr != nil {
			t.Fatal(chmodErr)
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			t.Fatal(chmodErr)
		}
	})
	Zero(data)
	if err == nil || !strings.Contains(err.Error(), "pathname changed") {
		t.Fatalf("restored mode was not detected through change time: %v", err)
	}
}

func TestReadPrivateKeyFileRejectsUnsafeOrAmbiguousSources(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	for _, test := range []struct {
		name  string
		setup func(string) error
		want  string
	}{
		{name: "group-readable", setup: func(path string) error { return os.WriteFile(path, privateKey, 0o640) }, want: "private regular file"},
		{name: "not-pem", setup: func(path string) error { return os.WriteFile(path, []byte("not a key"), 0o600) }, want: "exactly one PEM block"},
		{name: "multiple-pem-blocks", setup: func(path string) error {
			return os.WriteFile(path, append(append([]byte(nil), privateKey...), privateKey...), 0o600)
		}, want: "exactly one PEM block"},
		{name: "oversized", setup: func(path string) error {
			return os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxPrivateKeySize+1), 0o600)
		}, want: "between 1 and"},
		{name: "directory", setup: func(path string) error { return os.Mkdir(path, 0o700) }, want: "regular file"},
		{name: "hard-link", setup: func(path string) error {
			if err := os.WriteFile(path, privateKey, 0o600); err != nil {
				return err
			}
			return os.Link(path, path+".link")
		}, want: "exactly one hard link"},
		{name: "symlink", setup: func(path string) error {
			target := path + ".target"
			if err := os.WriteFile(target, privateKey, 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}, want: "open GitHub App private key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := test.setup(path); err != nil {
				t.Fatal(err)
			}
			data, _, err := ReadPrivateKeyFile(path)
			Zero(data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadPrivateKeyFile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestZeroPrivateKeyClearsExportedSecretIntegers(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	defer Zero(privateKey)
	key, err := ParsePrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key.Precompute()
	secretWords := [][]big.Word{key.D.Bits()}
	for _, prime := range key.Primes {
		secretWords = append(secretWords, prime.Bits())
	}
	for _, value := range []*big.Int{key.Precomputed.Dp, key.Precomputed.Dq, key.Precomputed.Qinv} {
		secretWords = append(secretWords, value.Bits())
	}

	ZeroPrivateKey(key)
	if key.D.Sign() != 0 {
		t.Fatal("private exponent was retained")
	}
	for _, prime := range key.Primes {
		if prime.Sign() != 0 {
			t.Fatal("private prime was retained")
		}
	}
	for _, words := range secretWords {
		for _, word := range words {
			if word != 0 {
				t.Fatal("private integer backing storage was not cleared")
			}
		}
	}
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
