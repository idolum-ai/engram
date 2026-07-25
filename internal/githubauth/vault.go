package githubauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/idolum-ai/engram/internal/atomicfile"
)

const (
	vaultVersion      = 1
	maxVaultBytes     = 2 << 20
	maxPrivateKeySize = 128 << 10
	minPassphraseSize = 12
	pbkdf2Iterations  = 600_000
	pbkdf2KeyBytes    = 32
)

var ErrUnlock = errors.New("GitHub App credential could not be unlocked")

type KDFParameters struct {
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	KeyBytes   int    `json:"key_bytes"`
	Salt       string `json:"salt"`
}

type EncryptedPrivateKey struct {
	Ciphertext string        `json:"ciphertext"`
	Nonce      string        `json:"nonce"`
	KDF        KDFParameters `json:"kdf"`
}

type App struct {
	Alias             string              `json:"alias"`
	AppID             int64               `json:"app_id"`
	InstallationID    int64               `json:"installation_id"`
	TelegramUnlock    bool                `json:"telegram_unlock,omitempty"`
	PublicFingerprint string              `json:"public_fingerprint"`
	CreatedAt         time.Time           `json:"created_at"`
	PrivateKey        EncryptedPrivateKey `json:"private_key"`
}

type persistedVault struct {
	Version int   `json:"version"`
	Apps    []App `json:"apps,omitempty"`
}

type Vault struct {
	mu    sync.Mutex
	path  string
	state persistedVault
}

func OpenVault(path string) (*Vault, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("GitHub credential vault path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create GitHub credential directory: %w", err)
	}
	vault := &Vault{path: path, state: persistedVault{Version: vaultVersion}}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		if err := vault.saveLocked(); err != nil {
			return nil, err
		}
		return vault, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open GitHub credential vault: %w", err)
	}
	if err := validatePrivateFile(file, "GitHub credential vault"); err != nil {
		_ = file.Close()
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxVaultBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read GitHub credential vault: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close GitHub credential vault: %w", closeErr)
	}
	if len(data) > maxVaultBytes {
		return nil, fmt.Errorf("GitHub credential vault exceeds %d bytes", maxVaultBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("GitHub credential vault is empty")
	}
	if err := json.Unmarshal(data, &vault.state); err != nil {
		return nil, fmt.Errorf("parse GitHub credential vault: %w", err)
	}
	if vault.state.Version > vaultVersion {
		return nil, fmt.Errorf("GitHub credential vault schema version %d is newer than supported version %d", vault.state.Version, vaultVersion)
	}
	vault.state.Version = vaultVersion
	if err := validateVault(vault.state); err != nil {
		return nil, err
	}
	return vault, nil
}

func (v *Vault) Path() string {
	if v == nil {
		return ""
	}
	return v.path
}

// Reload observes enrollment changes made by another Engram CLI process.
// Existing in-memory leases remain the broker's responsibility.
func (v *Vault) Reload() error {
	if v == nil {
		return fmt.Errorf("GitHub credential vault is unavailable")
	}
	file, err := os.OpenFile(v.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open GitHub credential vault: %w", err)
	}
	if err := validatePrivateFile(file, "GitHub credential vault"); err != nil {
		_ = file.Close()
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxVaultBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read GitHub credential vault: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close GitHub credential vault: %w", closeErr)
	}
	if len(data) > maxVaultBytes || len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("GitHub credential vault is invalid")
	}
	var state persistedVault
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse GitHub credential vault: %w", err)
	}
	if state.Version > vaultVersion {
		return fmt.Errorf("GitHub credential vault schema version %d is newer than supported version %d", state.Version, vaultVersion)
	}
	state.Version = vaultVersion
	if err := validateVault(state); err != nil {
		return err
	}
	v.mu.Lock()
	v.state = state
	v.mu.Unlock()
	return nil
}

func (v *Vault) List() []App {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	apps := make([]App, len(v.state.Apps))
	copy(apps, v.state.Apps)
	for i := range apps {
		apps[i].PrivateKey = EncryptedPrivateKey{}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Alias < apps[j].Alias })
	return apps
}

func (v *Vault) Get(alias string) (App, bool) {
	if v == nil {
		return App{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, app := range v.state.Apps {
		if app.Alias == alias {
			app.PrivateKey = EncryptedPrivateKey{}
			return app, true
		}
	}
	return App{}, false
}

func (v *Vault) Add(alias string, appID, installationID int64, privateKeyPEM, passphrase []byte, telegramUnlock bool) (App, bool, error) {
	alias = strings.TrimSpace(alias)
	if err := validateAlias(alias); err != nil {
		return App{}, false, err
	}
	if appID <= 0 || installationID <= 0 {
		return App{}, false, fmt.Errorf("app ID and installation ID must be positive")
	}
	if len(privateKeyPEM) == 0 || len(privateKeyPEM) > maxPrivateKeySize {
		return App{}, false, fmt.Errorf("GitHub App private key must be between 1 and %d bytes", maxPrivateKeySize)
	}
	if len(passphrase) < minPassphraseSize {
		return App{}, false, fmt.Errorf("passphrase must be at least %d bytes", minPassphraseSize)
	}
	key, err := ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return App{}, false, err
	}
	fingerprint, err := PublicFingerprint(key)
	if err != nil {
		return App{}, false, err
	}
	encrypted, err := encryptPrivateKey(alias, appID, installationID, privateKeyPEM, passphrase)
	if err != nil {
		return App{}, false, err
	}
	item := App{
		Alias:             alias,
		AppID:             appID,
		InstallationID:    installationID,
		TelegramUnlock:    telegramUnlock,
		PublicFingerprint: fingerprint,
		CreatedAt:         time.Now().UTC(),
		PrivateKey:        encrypted,
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	previous := cloneVault(v.state)
	created := true
	for index := range v.state.Apps {
		if v.state.Apps[index].Alias == alias {
			v.state.Apps[index] = item
			created = false
			if err := v.saveLocked(); err != nil {
				if !atomicfile.ReachedReplacement(err) {
					v.state = previous
				}
				return App{}, false, err
			}
			item.PrivateKey = EncryptedPrivateKey{}
			return item, created, nil
		}
	}
	v.state.Apps = append(v.state.Apps, item)
	if err := v.saveLocked(); err != nil {
		if !atomicfile.ReachedReplacement(err) {
			v.state = previous
		}
		return App{}, false, err
	}
	item.PrivateKey = EncryptedPrivateKey{}
	return item, created, nil
}

func (v *Vault) Remove(alias string) (bool, error) {
	if v == nil {
		return false, fmt.Errorf("GitHub credential vault is unavailable")
	}
	alias = strings.TrimSpace(alias)
	v.mu.Lock()
	defer v.mu.Unlock()
	for index := range v.state.Apps {
		if v.state.Apps[index].Alias != alias {
			continue
		}
		previous := cloneVault(v.state)
		v.state.Apps = append(v.state.Apps[:index], v.state.Apps[index+1:]...)
		if err := v.saveLocked(); err != nil {
			if !atomicfile.ReachedReplacement(err) {
				v.state = previous
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (v *Vault) Unlock(alias string, passphrase []byte) ([]byte, App, error) {
	if v == nil {
		return nil, App{}, ErrUnlock
	}
	v.mu.Lock()
	var stored App
	found := false
	for _, app := range v.state.Apps {
		if app.Alias == strings.TrimSpace(alias) {
			stored = app
			found = true
			break
		}
	}
	v.mu.Unlock()
	if !found {
		return nil, App{}, ErrUnlock
	}
	plaintext, err := decryptPrivateKey(stored, passphrase)
	if err != nil {
		return nil, App{}, ErrUnlock
	}
	if _, err := ParsePrivateKey(plaintext); err != nil {
		zeroBytes(plaintext)
		return nil, App{}, ErrUnlock
	}
	public := stored
	public.PrivateKey = EncryptedPrivateKey{}
	return plaintext, public, nil
}

func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("GitHub App private key must contain exactly one PEM block")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub App RSA private key: %w", err)
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub App private key: %w", err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("GitHub App private key must be RSA")
		}
	default:
		return nil, fmt.Errorf("unsupported GitHub App private key PEM type %q", block.Type)
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("validate GitHub App private key: %w", err)
	}
	return key, nil
}

func PublicFingerprint(key *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal GitHub App public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func encryptPrivateKey(alias string, appID, installationID int64, plaintext, passphrase []byte) (EncryptedPrivateKey, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return EncryptedPrivateKey{}, fmt.Errorf("generate credential salt: %w", err)
	}
	params := KDFParameters{
		Algorithm:  "pbkdf2-hmac-sha256",
		Iterations: pbkdf2Iterations,
		KeyBytes:   pbkdf2KeyBytes,
		Salt:       base64.RawStdEncoding.EncodeToString(salt),
	}
	key := derivePBKDF2SHA256(passphrase, salt, params.Iterations, params.KeyBytes)
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return EncryptedPrivateKey{}, fmt.Errorf("initialize credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedPrivateKey{}, fmt.Errorf("initialize credential authentication: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return EncryptedPrivateKey{}, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, credentialAAD(alias, appID, installationID))
	return EncryptedPrivateKey{
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		KDF:        params,
	}, nil
}

func decryptPrivateKey(app App, passphrase []byte) ([]byte, error) {
	params := app.PrivateKey.KDF
	if params.Algorithm != "pbkdf2-hmac-sha256" ||
		params.Iterations < 100_000 || params.Iterations > 2_000_000 ||
		params.KeyBytes != 32 {
		return nil, ErrUnlock
	}
	salt, err := base64.RawStdEncoding.DecodeString(params.Salt)
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return nil, ErrUnlock
	}
	nonce, err := base64.RawStdEncoding.DecodeString(app.PrivateKey.Nonce)
	if err != nil {
		return nil, ErrUnlock
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(app.PrivateKey.Ciphertext)
	if err != nil || len(ciphertext) > maxPrivateKeySize+64 {
		return nil, ErrUnlock
	}
	key := derivePBKDF2SHA256(passphrase, salt, params.Iterations, params.KeyBytes)
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrUnlock
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return nil, ErrUnlock
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, credentialAAD(app.Alias, app.AppID, app.InstallationID))
	if err != nil {
		return nil, ErrUnlock
	}
	return plaintext, nil
}

func credentialAAD(alias string, appID, installationID int64) []byte {
	return []byte(fmt.Sprintf("engram-github-app-v%d\x00%s\x00%d\x00%d", vaultVersion, alias, appID, installationID))
}

func validateVault(state persistedVault) error {
	seen := map[string]bool{}
	for _, app := range state.Apps {
		if err := validateAlias(app.Alias); err != nil {
			return fmt.Errorf("invalid GitHub credential vault: %w", err)
		}
		if seen[app.Alias] {
			return fmt.Errorf("invalid GitHub credential vault: duplicate alias %q", app.Alias)
		}
		seen[app.Alias] = true
		if app.AppID <= 0 || app.InstallationID <= 0 || strings.TrimSpace(app.PublicFingerprint) == "" ||
			strings.TrimSpace(app.PrivateKey.Ciphertext) == "" || strings.TrimSpace(app.PrivateKey.Nonce) == "" {
			return fmt.Errorf("invalid GitHub credential vault entry %q", app.Alias)
		}
	}
	return nil
}

func validateAlias(alias string) error {
	if len(alias) == 0 || len(alias) > 32 {
		return fmt.Errorf("GitHub App alias must be between 1 and 32 bytes")
	}
	for index, r := range alias {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (index > 0 && (r == '-' || r == '_')) {
			continue
		}
		return fmt.Errorf("GitHub App alias must use lowercase letters, digits, hyphens, or underscores")
	}
	return nil
}

func validatePrivateFile(file *os.File, label string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", label, err)
	}
	stat, ownerOK := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownerOK || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a private regular file owned by uid %d", label, os.Geteuid())
	}
	return nil
}

func (v *Vault) saveLocked() error {
	v.state.Version = vaultVersion
	data, err := json.MarshalIndent(v.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode GitHub credential vault: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxVaultBytes {
		return fmt.Errorf("GitHub credential vault exceeds %d bytes", maxVaultBytes)
	}
	if err := atomicfile.Write(v.path, data); err != nil {
		return fmt.Errorf("save GitHub credential vault: %w", err)
	}
	return nil
}

func cloneVault(state persistedVault) persistedVault {
	out := state
	out.Apps = append([]App(nil), state.Apps...)
	return out
}

func zeroBytes(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

func derivePBKDF2SHA256(password, salt []byte, iterations, keyBytes int) []byte {
	const hashBytes = sha256.Size
	blocks := (keyBytes + hashBytes - 1) / hashBytes
	derived := make([]byte, 0, blocks*hashBytes)
	var blockIndex [4]byte
	for block := 1; block <= blocks; block++ {
		blockIndex[0] = byte(block >> 24)
		blockIndex[1] = byte(block >> 16)
		blockIndex[2] = byte(block >> 8)
		blockIndex[3] = byte(block)
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(blockIndex[:])
		u := mac.Sum(nil)
		accumulator := append([]byte(nil), u...)
		for iteration := 1; iteration < iterations; iteration++ {
			mac.Reset()
			_, _ = mac.Write(u)
			next := mac.Sum(nil)
			zeroBytes(u)
			u = next
			for index := range accumulator {
				accumulator[index] ^= u[index]
			}
		}
		zeroBytes(u)
		derived = append(derived, accumulator...)
		zeroBytes(accumulator)
	}
	return derived[:keyBytes]
}

func Zero(data []byte) {
	zeroBytes(data)
}
