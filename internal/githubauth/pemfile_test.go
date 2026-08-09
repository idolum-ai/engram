package githubauth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
