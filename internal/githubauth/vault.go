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
	"math/big"
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
	vaultVersion              = 2
	credentialIdentityVersion = 2
	maxVaultBytes             = 2 << 20
	maxPrivateKeySize         = 128 << 10
	minPassphraseSize         = 12
	pbkdf2Iterations          = 600_000
	pbkdf2KeyBytes            = 32
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
	Alias                     string              `json:"alias"`
	AppID                     int64               `json:"app_id"`
	InstallationID            int64               `json:"installation_id"`
	InstallationIDs           []int64             `json:"installation_ids,omitempty"`
	CredentialIdentityVersion int                 `json:"credential_identity_version,omitempty"`
	TelegramUnlock            bool                `json:"telegram_unlock,omitempty"`
	PublicFingerprint         string              `json:"public_fingerprint"`
	CreatedAt                 time.Time           `json:"created_at"`
	PrivateKey                EncryptedPrivateKey `json:"private_key"`
	selectedInstallationID    int64               `json:"-"`
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
		apps[i].InstallationIDs = apps[i].Installations()
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
			app.InstallationIDs = app.Installations()
			return app, true
		}
	}
	return App{}, false
}

func (v *Vault) Add(alias string, appID, installationID int64, privateKeyPEM, passphrase []byte, telegramUnlock bool) (App, bool, error) {
	return v.AddInstallations(alias, appID, []int64{installationID}, privateKeyPEM, passphrase, telegramUnlock)
}

// AddInstallations atomically enrolls or replaces one App credential and the
// complete set of installation IDs that may use it. New ciphertext binds the
// complete set as authenticated metadata. Version-1 entries remain readable
// without being rewritten until the operator explicitly re-enrolls them.
func (v *Vault) AddInstallations(alias string, appID int64, installationIDs []int64, privateKeyPEM, passphrase []byte, telegramUnlock bool) (App, bool, error) {
	alias = strings.TrimSpace(alias)
	if err := validateAlias(alias); err != nil {
		return App{}, false, err
	}
	installationIDs, err := normalizeInstallationIDs(installationIDs)
	if appID <= 0 || err != nil {
		if err != nil {
			return App{}, false, err
		}
		return App{}, false, fmt.Errorf("app ID must be positive")
	}
	if len(privateKeyPEM) == 0 || len(privateKeyPEM) > maxPrivateKeySize {
		return App{}, false, fmt.Errorf("GitHub App private key must be between 1 and %d bytes", maxPrivateKeySize)
	}
	if len(passphrase) < minPassphraseSize {
		return App{}, false, fmt.Errorf("passphrase must be at least %d bytes", minPassphraseSize)
	}
	fingerprint, err := PrivateKeyFingerprint(privateKeyPEM)
	if err != nil {
		return App{}, false, err
	}
	credentialInstallationID := installationIDs[0]
	encrypted, err := encryptPrivateKeyForInstallations(alias, appID, installationIDs, privateKeyPEM, passphrase)
	if err != nil {
		return App{}, false, err
	}
	item := App{
		Alias:                     alias,
		AppID:                     appID,
		InstallationID:            credentialInstallationID,
		InstallationIDs:           append([]int64(nil), installationIDs...),
		CredentialIdentityVersion: credentialIdentityVersion,
		TelegramUnlock:            telegramUnlock,
		PublicFingerprint:         fingerprint,
		CreatedAt:                 time.Now().UTC(),
		PrivateKey:                encrypted,
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

// Installations returns the canonical installation set. A missing plural field
// is the version-1 single-installation representation and is migrated in memory
// without rewriting the encrypted vault.
func (a App) Installations() []int64 {
	if len(a.InstallationIDs) == 0 {
		if a.InstallationID <= 0 {
			return nil
		}
		return []int64{a.InstallationID}
	}
	out := append([]int64(nil), a.InstallationIDs...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// EffectiveInstallationID is the one installation bound to this request,
// grant, lease, or mint. App credentials may contain several installations,
// but a token never does.
func (a App) EffectiveInstallationID() int64 {
	if a.selectedInstallationID > 0 {
		return a.selectedInstallationID
	}
	return a.InstallationID
}

// SelectInstallation binds a public enrollment value to exactly one enrolled
// installation without changing the credential ciphertext identity.
func (a App) SelectInstallation(installationID int64) (App, error) {
	installations := a.Installations()
	if installationID == 0 {
		if len(installations) != 1 {
			return App{}, fmt.Errorf("GitHub App %q has %d installations (%s); pass --installation-id to select exactly one", a.Alias, len(installations), formatInstallationIDs(installations))
		}
		installationID = installations[0]
	}
	for _, candidate := range installations {
		if candidate == installationID {
			a.InstallationIDs = installations
			a.selectedInstallationID = installationID
			return a, nil
		}
	}
	return App{}, fmt.Errorf("GitHub App %q does not enroll installation %d; enrolled installations: %s", a.Alias, installationID, formatInstallationIDs(installations))
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
	key, err := ParsePrivateKey(plaintext)
	if err != nil {
		zeroBytes(plaintext)
		return nil, App{}, ErrUnlock
	}
	ZeroPrivateKey(key)
	public := stored
	public.PrivateKey = EncryptedPrivateKey{}
	public.InstallationIDs = public.Installations()
	return plaintext, public, nil
}

func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("GitHub App private key must contain exactly one PEM block")
	}
	defer zeroBytes(block.Bytes)
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
		ZeroPrivateKey(key)
		return nil, fmt.Errorf("validate GitHub App private key: %w", err)
	}
	return key, nil
}

// PrivateKeyFingerprint derives the public identity of one validated key and
// clears the parsed private representation before returning.
func PrivateKeyFingerprint(data []byte) (string, error) {
	key, err := ParsePrivateKey(data)
	if err != nil {
		return "", err
	}
	defer ZeroPrivateKey(key)
	return PublicFingerprint(key)
}

func PublicFingerprint(key *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal GitHub App public key: %w", err)
	}
	defer zeroBytes(der)
	sum := sha256.Sum256(der)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// ZeroPrivateKey clears the exported secret big integers in an RSA key on a
// best-effort basis. Go does not provide guarantees about copies made by the
// runtime or cryptographic implementation.
func ZeroPrivateKey(key *rsa.PrivateKey) {
	if key == nil {
		return
	}
	zeroBigInt(key.D)
	for _, prime := range key.Primes {
		zeroBigInt(prime)
	}
	zeroBigInt(key.Precomputed.Dp)
	zeroBigInt(key.Precomputed.Dq)
	zeroBigInt(key.Precomputed.Qinv)
	for index := range key.Precomputed.CRTValues {
		zeroBigInt(key.Precomputed.CRTValues[index].Exp)
		zeroBigInt(key.Precomputed.CRTValues[index].Coeff)
		zeroBigInt(key.Precomputed.CRTValues[index].R)
	}
	key.Precomputed = rsa.PrecomputedValues{}
}

func zeroBigInt(value *big.Int) {
	if value == nil {
		return
	}
	words := value.Bits()
	for index := range words {
		words[index] = 0
	}
	value.SetInt64(0)
}

func encryptPrivateKey(alias string, appID, installationID int64, plaintext, passphrase []byte) (EncryptedPrivateKey, error) {
	return encryptPrivateKeyWithAAD(credentialAAD(alias, appID, installationID), plaintext, passphrase)
}

func encryptPrivateKeyForInstallations(alias string, appID int64, installationIDs []int64, plaintext, passphrase []byte) (EncryptedPrivateKey, error) {
	return encryptPrivateKeyWithAAD(credentialInstallationSetAAD(alias, appID, installationIDs), plaintext, passphrase)
}

func encryptPrivateKeyWithAAD(aad, plaintext, passphrase []byte) (EncryptedPrivateKey, error) {
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
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
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
	aad := credentialAAD(app.Alias, app.AppID, app.InstallationID)
	if app.CredentialIdentityVersion == credentialIdentityVersion {
		aad = credentialInstallationSetAAD(app.Alias, app.AppID, app.Installations())
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrUnlock
	}
	return plaintext, nil
}

func credentialAAD(alias string, appID, installationID int64) []byte {
	return []byte(fmt.Sprintf("engram-github-app-v1\x00%s\x00%d\x00%d", alias, appID, installationID))
}

func credentialInstallationSetAAD(alias string, appID int64, installationIDs []int64) []byte {
	return []byte(fmt.Sprintf("engram-github-app-v2\x00%s\x00%d\x00%s", alias, appID, formatInstallationIDs(installationIDs)))
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
		installations, err := normalizeInstallationIDs(app.Installations())
		if err != nil {
			return fmt.Errorf("invalid GitHub credential vault entry %q: %w", app.Alias, err)
		}
		if len(app.InstallationIDs) != 0 && len(installations) != len(app.InstallationIDs) {
			return fmt.Errorf("invalid GitHub credential vault entry %q: duplicate installation ID", app.Alias)
		}
		anchorFound := false
		for _, installationID := range installations {
			if installationID == app.InstallationID {
				anchorFound = true
				break
			}
		}
		if app.CredentialIdentityVersion != 0 && app.CredentialIdentityVersion != credentialIdentityVersion {
			return fmt.Errorf("invalid GitHub credential vault entry %q: unsupported credential identity version", app.Alias)
		}
		if app.CredentialIdentityVersion == 0 && len(app.InstallationIDs) != 0 &&
			(len(app.InstallationIDs) != 1 || app.InstallationIDs[0] != app.InstallationID) {
			return fmt.Errorf("invalid GitHub credential vault entry %q: legacy credential cannot enroll additional installations without re-enrollment", app.Alias)
		}
		if app.CredentialIdentityVersion == credentialIdentityVersion && len(app.InstallationIDs) == 0 {
			return fmt.Errorf("invalid GitHub credential vault entry %q: missing authenticated installation set", app.Alias)
		}
		if app.AppID <= 0 || app.InstallationID <= 0 || !anchorFound || strings.TrimSpace(app.PublicFingerprint) == "" ||
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
	for index := range out.Apps {
		out.Apps[index].InstallationIDs = append([]int64(nil), out.Apps[index].InstallationIDs...)
	}
	return out
}

func normalizeInstallationIDs(values []int64) ([]int64, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one GitHub App installation ID is required")
	}
	if len(values) > 100 {
		return nil, fmt.Errorf("at most 100 GitHub App installation IDs may be enrolled under one alias")
	}
	seen := make(map[int64]bool, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, fmt.Errorf("GitHub App installation IDs must be positive")
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func formatInstallationIDs(values []int64) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = fmt.Sprintf("%d", value)
	}
	return strings.Join(parts, ", ")
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
