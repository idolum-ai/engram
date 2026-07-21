package templates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	currentVersion   = 1
	MaxTemplates     = 64
	MaxNameBytes     = 32
	MaxBodyBytes     = 4000
	MaxExpandedBytes = 16 * 1024
	maxStateBytes    = 512 * 1024
)

type Template struct {
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
}

type persistedState struct {
	Version   int        `json:"version"`
	Templates []Template `json:"templates,omitempty"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	state persistedState
}

type atomicWriteError struct {
	err      error
	replaced bool
}

func (e *atomicWriteError) Error() string { return e.err.Error() }
func (e *atomicWriteError) Unwrap() error { return e.err }

func PersistenceReachedReplacement(err error) bool {
	var target *atomicWriteError
	return errors.As(err, &target) && target.replaced
}

func Open(path string) (*Store, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("template state path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create template state directory: %w", err)
	}
	store := &Store{path: path, state: persistedState{Version: currentVersion}}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return store, store.saveLocked()
	}
	if err != nil {
		return nil, fmt.Errorf("open template state: %w", err)
	}
	defer file.Close()
	if err := validateOpenedFile(file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read template state: %w", err)
	}
	if len(data) > maxStateBytes {
		return nil, fmt.Errorf("template state exceeds %d bytes", maxStateBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("template state is empty")
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("parse template state: %w", err)
	}
	if store.state.Version > currentVersion {
		return nil, fmt.Errorf("template state schema version %d is newer than supported version %d", store.state.Version, currentVersion)
	}
	store.state.Version = currentVersion
	if err := validateState(store.state); err != nil {
		return nil, err
	}
	return store, nil
}

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

// ExportJSON returns one consistent snapshot in the same format used on disk.
func (s *Store) ExportJSON() ([]byte, error) {
	s.mu.Lock()
	state := cloneState(s.state)
	s.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode template export: %w", err)
	}
	return append(data, '\n'), nil
}

func (s *Store) Put(name, body string, now time.Time) (Template, bool, error) {
	name = strings.TrimSpace(name)
	body = strings.TrimSpace(body)
	if err := validateFields(name, body); err != nil {
		return Template{}, false, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	item := Template{Name: name, Body: body, UpdatedAt: now.UTC()}
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

// Expand performs one pass. Template bodies are appended verbatim and are
// never scanned again, so remembered text cannot activate another template.
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
	for index := 0; index < len(text); {
		if strings.HasPrefix(text[index:], "{{") {
			end := strings.Index(text[index+2:], "}}")
			if end < 0 {
				if err := appendValue(text[index:]); err != nil {
					return "", nil, err
				}
				break
			}
			literal := "{" + text[index+2:index+2+end] + "}"
			if err := appendValue(literal); err != nil {
				return "", nil, err
			}
			index += 2 + end + 2
			continue
		}
		if text[index] != '{' {
			if err := appendValue(text[index : index+1]); err != nil {
				return "", nil, err
			}
			index++
			continue
		}
		end := strings.IndexByte(text[index+1:], '}')
		if end < 0 {
			if err := appendValue(text[index:]); err != nil {
				return "", nil, err
			}
			break
		}
		name := text[index+1 : index+1+end]
		if validateName(name) != nil {
			literal := text[index : index+1+end+1]
			if err := appendValue(literal); err != nil {
				return "", nil, err
			}
			index += end + 2
			continue
		}
		body, ok := byName[name]
		if !ok {
			// Hyphenated and underscored names are explicit template syntax. A
			// simple unknown word remains literal so ordinary code such as {err}
			// does not become invalid terminal input.
			if strings.ContainsAny(name, "-_") {
				return "", nil, fmt.Errorf("unknown template {%s}; use /remember to list templates", name)
			}
			literal := text[index : index+1+end+1]
			if err := appendValue(literal); err != nil {
				return "", nil, err
			}
			index += end + 2
			continue
		}
		if err := appendValue(body); err != nil {
			return "", nil, err
		}
		if !usedSet[name] {
			usedSet[name] = true
			used = append(used, name)
		}
		index += end + 2
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
		return fmt.Errorf("template body must contain 1 to %d bytes without NUL", MaxBodyBytes)
	}
	return nil
}

func validateName(name string) error {
	if len(name) == 0 || len(name) > MaxNameBytes {
		return fmt.Errorf("template name must contain 1 to %d bytes", MaxNameBytes)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("template name may contain lowercase letters, digits, '-' and '_'")
	}
	return nil
}

func (s *Store) saveLocked() error {
	s.state.Version = currentVersion
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode template state: %w", err)
	}
	dir := filepath.Dir(s.path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return &atomicWriteError{err: fmt.Errorf("create template state temporary file: %w", err)}
	}
	temporary := file.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return &atomicWriteError{err: errors.Join(fmt.Errorf("chmod template state: %w", err), file.Close())}
	}
	if _, err := file.Write(data); err != nil {
		return &atomicWriteError{err: errors.Join(fmt.Errorf("write template state: %w", err), file.Close())}
	}
	if err := file.Sync(); err != nil {
		return &atomicWriteError{err: errors.Join(fmt.Errorf("sync template state: %w", err), file.Close())}
	}
	if err := file.Close(); err != nil {
		return &atomicWriteError{err: fmt.Errorf("close template state: %w", err)}
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return &atomicWriteError{err: fmt.Errorf("replace template state: %w", err)}
	}
	renamed = true
	if err := syncParentDir(dir); err != nil {
		return &atomicWriteError{err: err, replaced: true}
	}
	return nil
}

func syncParentDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open template state directory for sync: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if runtime.GOOS == "darwin" && (errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP)) {
		syncErr = nil
	}
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync template state directory: %w", err)
	}
	return nil
}

func cloneState(in persistedState) persistedState {
	out := in
	out.Templates = append([]Template(nil), in.Templates...)
	return out
}
