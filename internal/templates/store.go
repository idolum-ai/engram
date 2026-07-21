package templates

import (
	"encoding/json"
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
	currentVersion   = 1
	MaxTemplates     = 64
	MaxNameBytes     = 32
	MaxBodyBytes     = 3800
	MaxExpandedBytes = 16 * 1024
	maxStateBytes    = 512 * 1024
)

type Template struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type persistedState struct {
	Version   int        `json:"version"`
	Templates []Template `json:"templates,omitempty"`
}

type Store struct {
	mu              sync.Mutex
	path            string
	recoveredPath   string
	recoveryWarning error
	state           persistedState
	writeFile       func(string, []byte) error
}

type validationError struct{ message string }

func (e *validationError) Error() string { return e.message }

func IsValidationError(err error) bool {
	var target *validationError
	return errors.As(err, &target)
}

func PersistenceReachedReplacement(err error) bool {
	return atomicfile.ReachedReplacement(err)
}

func Open(path string) (*Store, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("template state path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create template state directory: %w", err)
	}
	store := &Store{
		path:      path,
		state:     persistedState{Version: currentVersion},
		writeFile: atomicfile.Write,
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		store.recoveredPath = latestRecoveryBackup(path)
		return store, store.saveLocked()
	}
	if err != nil {
		return nil, fmt.Errorf("open template state: %w", err)
	}
	if err := validateOpenedFile(file); err != nil {
		file.Close()
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("read template state: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close template state: %w", closeErr)
	}
	if len(data) > maxStateBytes {
		return recoverCorrupt(store, fmt.Errorf("template state exceeds %d bytes", maxStateBytes))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return recoverCorrupt(store, fmt.Errorf("template state is empty"))
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return recoverCorrupt(store, fmt.Errorf("parse template state: %w", err))
	}
	if envelope.Version > currentVersion {
		return nil, fmt.Errorf("template state schema version %d is newer than supported version %d", envelope.Version, currentVersion)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return recoverCorrupt(store, fmt.Errorf("parse template state: %w", err))
	}
	store.state.Version = currentVersion
	if err := validateState(store.state); err != nil {
		return recoverCorrupt(store, err)
	}
	return store, nil
}

func recoverCorrupt(store *Store, cause error) (*Store, error) {
	backup := store.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(store.path, backup); err != nil {
		return nil, fmt.Errorf("%w; preserve corrupt templates: %w", cause, err)
	}
	if err := atomicfile.SyncDir(filepath.Dir(store.path)); err != nil {
		return nil, fmt.Errorf("%w; backup at %s; sync backup directory: %w", cause, backup, err)
	}
	store.state = persistedState{Version: currentVersion}
	store.recoveredPath = backup
	if err := store.saveLocked(); err != nil {
		if atomicfile.ReachedReplacement(err) {
			store.recoveryWarning = err
			return store, nil
		}
		return nil, fmt.Errorf("%w; backup at %s; initialize replacement: %w", cause, backup, err)
	}
	return store, nil
}

func latestRecoveryBackup(path string) string {
	matches, _ := filepath.Glob(path + ".corrupt-*")
	for index := len(matches) - 1; index >= 0; index-- {
		file, err := os.OpenFile(matches[index], os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		validationErr := validateOpenedFile(file)
		closeErr := file.Close()
		if validationErr == nil && closeErr == nil {
			return matches[index]
		}
	}
	return ""
}

func (s *Store) RecoveredPath() string { return s.recoveredPath }

func (s *Store) RecoveryWarning() error { return s.recoveryWarning }

func validateOpenedFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat template state: %w", err)
	}
	stat, ownerOK := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownerOK || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("template state must be a private regular file owned by uid %d", os.Geteuid())
	}
	return nil
}

func (s *Store) List() []Template {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Template(nil), s.state.Templates...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Get(name string) (Template, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.state.Templates {
		if item.Name == name {
			return item, true
		}
	}
	return Template{}, false
}

func (s *Store) Put(name, body string) (Template, bool, error) {
	if err := validateFields(name, body); err != nil {
		return Template{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	item := Template{Name: name, Body: body}
	created := true
	for index := range s.state.Templates {
		if s.state.Templates[index].Name == name {
			s.state.Templates[index] = item
			created = false
			if err := s.saveLocked(); err != nil {
				if !PersistenceReachedReplacement(err) {
					s.state = previous
				}
				return item, created, err
			}
			return item, created, nil
		}
	}
	if len(s.state.Templates) >= MaxTemplates {
		return Template{}, false, fmt.Errorf("template limit reached")
	}
	s.state.Templates = append(s.state.Templates, item)
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
		}
		return item, created, err
	}
	return item, created, nil
}

func (s *Store) Forget(name string) (Template, bool, error) {
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, item := range s.state.Templates {
		if item.Name != name {
			continue
		}
		previous := cloneState(s.state)
		s.state.Templates = append(s.state.Templates[:index], s.state.Templates[index+1:]...)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
			}
			return item, true, err
		}
		return item, true, nil
	}
	return Template{}, false, nil
}

// Expand recognizes only the explicit {engram:name} namespace and performs one
// pass. Template bodies are never scanned again.
func (s *Store) Expand(text string) (string, []string, error) {
	s.mu.Lock()
	items := append([]Template(nil), s.state.Templates...)
	s.mu.Unlock()
	byName := make(map[string]string, len(items))
	for _, item := range items {
		byName[item.Name] = item.Body
	}

	var out strings.Builder
	usedSet := make(map[string]bool)
	var used []string
	appendValue := func(value string) error {
		if out.Len()+len(value) > MaxExpandedBytes {
			return fmt.Errorf("expanded input exceeds %d bytes", MaxExpandedBytes)
		}
		out.WriteString(value)
		return nil
	}
	const prefix = "{engram:"
	for index := 0; index < len(text); {
		relative := strings.Index(text[index:], prefix)
		if relative < 0 {
			if err := appendValue(text[index:]); err != nil {
				return "", nil, err
			}
			break
		}
		start := index + relative
		if err := appendValue(text[index:start]); err != nil {
			return "", nil, err
		}
		// Preserve nested source-language constructs such as ${engram:name}
		// and {{engram:name}} byte-for-byte.
		if start > 0 && (text[start-1] == '$' || text[start-1] == '{') {
			if err := appendValue("{"); err != nil {
				return "", nil, err
			}
			index = start + 1
			continue
		}
		end := strings.IndexByte(text[start+len(prefix):], '}')
		if end < 0 {
			if err := appendValue(text[start:]); err != nil {
				return "", nil, err
			}
			break
		}
		closeIndex := start + len(prefix) + end
		if closeIndex+1 < len(text) && text[closeIndex+1] == '}' {
			if err := appendValue(text[start : closeIndex+1]); err != nil {
				return "", nil, err
			}
			index = closeIndex + 1
			continue
		}
		name := text[start+len(prefix) : closeIndex]
		if validateName(name) != nil {
			if err := appendValue(text[start : closeIndex+1]); err != nil {
				return "", nil, err
			}
			index = closeIndex + 1
			continue
		}
		body, ok := byName[name]
		if !ok {
			return "", nil, fmt.Errorf("unknown template {engram:%s}; use /remember to list templates", name)
		}
		if err := appendValue(body); err != nil {
			return "", nil, err
		}
		if !usedSet[name] {
			usedSet[name] = true
			used = append(used, name)
		}
		index = closeIndex + 1
	}
	return out.String(), used, nil
}

func validateState(state persistedState) error {
	if len(state.Templates) > MaxTemplates {
		return fmt.Errorf("template state exceeds %d templates", MaxTemplates)
	}
	seen := make(map[string]bool, len(state.Templates))
	for _, item := range state.Templates {
		if err := validateFields(item.Name, item.Body); err != nil {
			return fmt.Errorf("invalid template %q: %w", item.Name, err)
		}
		if seen[item.Name] {
			return fmt.Errorf("duplicate template %q", item.Name)
		}
		seen[item.Name] = true
	}
	return nil
}

func validateFields(name, body string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxBodyBytes || strings.ContainsRune(body, '\x00') {
		return &validationError{message: fmt.Sprintf("template body must contain 1 to %d bytes without NUL", MaxBodyBytes)}
	}
	return nil
}

func validateName(name string) error {
	if len(name) == 0 || len(name) > MaxNameBytes {
		return &validationError{message: fmt.Sprintf("template name must contain 1 to %d bytes", MaxNameBytes)}
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return &validationError{message: "template name may contain lowercase letters, digits, '-' and '_'"}
	}
	return nil
}

func (s *Store) saveLocked() error {
	s.state.Version = currentVersion
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode template state: %w", err)
	}
	return s.writeFile(s.path, data)
}

func cloneState(in persistedState) persistedState {
	out := in
	out.Templates = append([]Template(nil), in.Templates...)
	return out
}
