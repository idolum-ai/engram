package cue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const currentVersion = 1
const maxStateBytes = 1 << 20

type persistedState struct {
	Version      int           `json:"version"`
	Cues         []Cue         `json:"cues,omitempty"`
	Candidates   []Candidate   `json:"candidates,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
	Suppressed   []string      `json:"suppressed,omitempty"`
}

type Snapshot struct {
	Cues         []Cue
	Candidates   []Candidate
	Observations int
}

type Store struct {
	mu     sync.Mutex
	path   string
	state  persistedState
	memory []memoryObservation
}

type memoryObservation struct {
	prompt        string
	signature     intentSignature
	featureHashes []string
	replaySafe    bool
}

type persistenceError struct {
	err      error
	replaced bool
}

func (e *persistenceError) Error() string { return e.err.Error() }
func (e *persistenceError) Unwrap() error { return e.err }

func PersistenceReachedReplacement(err error) bool {
	var target *persistenceError
	return errors.As(err, &target) && target.replaced
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("cue state path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create cue state directory: %w", err)
	}
	store := &Store{path: path, state: persistedState{Version: currentVersion}}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return store, store.saveLocked()
	}
	if err != nil {
		return nil, fmt.Errorf("read cue state: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat cue state: %w", err)
	}
	stat, ownerOK := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownerOK || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("cue state must be a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cue state: %w", err)
	}
	if len(data) > maxStateBytes {
		return nil, fmt.Errorf("cue state exceeds %d bytes", maxStateBytes)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return store, store.saveLocked()
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("parse cue state: %w", err)
	}
	if store.state.Version > currentVersion {
		return nil, fmt.Errorf("cue state schema version %d is newer than supported version %d", store.state.Version, currentVersion)
	}
	store.state.Version = currentVersion
	if err := validateState(store.state); err != nil {
		return nil, err
	}
	store.pruneLocked()
	return store, nil
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Cues:         append([]Cue(nil), s.state.Cues...),
		Candidates:   cloneCandidates(s.state.Candidates),
		Observations: len(s.state.Observations),
	}
}

func (s *Store) Observe(context Context, prompt string, now time.Time) (*Candidate, error) {
	prompt = strings.TrimSpace(prompt)
	if !promptLearnable(prompt) {
		return nil, nil
	}
	features := extractFeatures(context, prompt)
	if len(features) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	pHash := promptHash(prompt)
	hashes := make([]string, 0, len(features))
	for _, feature := range features {
		hash := featureHash(feature)
		hashes = append(hashes, hash)
	}
	signature := makeIntentSignature(prompt)

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := clonePersistedState(s.state)
	previousMemory := append([]memoryObservation(nil), s.memory...)
	s.state.Observations = append(s.state.Observations, Observation{PromptHash: pHash, FeatureHashes: hashes, At: now.UTC()})
	s.memory = append(s.memory, memoryObservation{
		prompt: prompt, signature: signature, featureHashes: append([]string(nil), hashes...), replaySafe: promptReplaySafe(prompt),
	})
	s.pruneLocked()
	intentCluster := s.intentClusterLocked(signature)

	var proposed *Candidate
	for _, feature := range features {
		fHash := featureHash(feature)
		support, intentUses, safeVariants := intentAssociation(intentCluster, fHash)
		exactSupport, exactUses, _ := associationCounts(s.state.Observations, pHash, fHash)
		exactQualified := promptReplaySafe(prompt) && exactSupport >= MinimumSupport && exactSupport*100/exactUses >= 75
		if exactQualified && (support < MinimumSupport || exactSupport*100/exactUses > support*100/max(1, intentUses)) {
			support, intentUses, safeVariants = exactSupport, exactUses, []string{prompt}
		}
		if support < MinimumSupport || support*100/intentUses < 75 {
			continue
		}
		representative := representativePrompt(safeVariants)
		if representative == "" {
			continue
		}
		id := pairID(digest(makeIntentSignature(representative).normalized), fHash)
		if index := s.candidateByIDLocked(id); index >= 0 {
			if s.state.Candidates[index].ProposalMessageID == 0 {
				s.state.Candidates[index].Support = support
				s.state.Candidates[index].ConfidencePercent = support * 100 / intentUses
				s.state.Candidates[index].Variants = append([]string(nil), safeVariants...)
				candidate := s.state.Candidates[index]
				proposed = &candidate
			}
			break
		}
		if s.knownLocked(id, feature.pattern, representative) {
			continue
		}
		candidate := Candidate{
			ID: id, Name: suggestCueName(representative), Pattern: feature.pattern, Prompt: representative,
			Variants: append([]string(nil), safeVariants...), FeatureKind: feature.kind,
			Support: support, ConfidencePercent: support * 100 / intentUses, CreatedAt: now.UTC(),
		}
		s.state.Candidates = append(s.state.Candidates, candidate)
		proposed = &candidate
		break
	}
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			s.memory = previousMemory
			return nil, err
		}
		return proposed, err
	}
	return proposed, nil
}

func (s *Store) Add(name, pattern, prompt string, now time.Time) (Cue, error) {
	name = strings.TrimSpace(name)
	pattern = strings.TrimSpace(pattern)
	prompt = strings.TrimSpace(prompt)
	if err := validateCueFields(name, pattern, prompt); err != nil {
		return Cue{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Cues) >= MaxActiveCues {
		return Cue{}, fmt.Errorf("cue limit reached")
	}
	for _, existing := range s.state.Cues {
		if existing.Name == name {
			return Cue{}, fmt.Errorf("cue name %q already exists", name)
		}
		if existing.Pattern == pattern && existing.Prompt == prompt {
			return existing, nil
		}
	}
	previous := clonePersistedState(s.state)
	created := Cue{ID: cueID(pattern, prompt), Name: name, Pattern: pattern, Prompt: prompt, CreatedAt: now.UTC()}
	s.state.Cues = append(s.state.Cues, created)
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
		}
		return created, err
	}
	return created, nil
}

func (s *Store) BindProposal(id string, chatID int64, messageID int) error {
	if chatID == 0 || messageID <= 0 {
		return fmt.Errorf("invalid cue proposal message")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := clonePersistedState(s.state)
	for index := range s.state.Candidates {
		if s.state.Candidates[index].ID == id {
			s.state.Candidates[index].ProposalChatID = chatID
			s.state.Candidates[index].ProposalMessageID = messageID
			if err := s.saveLocked(); err != nil {
				if !PersistenceReachedReplacement(err) {
					s.state = previous
				}
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("cue candidate not found")
}

func (s *Store) Accept(id string, chatID int64, messageID int, now time.Time) (Cue, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.candidateIndexLocked(id, chatID, messageID)
	if index < 0 {
		return Cue{}, false, nil
	}
	if len(s.state.Cues) >= MaxActiveCues {
		return Cue{}, false, fmt.Errorf("cue limit reached")
	}
	previous := clonePersistedState(s.state)
	candidate := s.state.Candidates[index]
	name := candidate.Name
	if name == "" {
		name = "cue-" + candidate.ID[:6]
	}
	created := Cue{
		ID: cueID(candidate.Pattern, candidate.Prompt), Name: s.uniqueCueNameLocked(name, candidate.ID),
		Pattern: candidate.Pattern, Prompt: candidate.Prompt, CreatedAt: now.UTC(),
	}
	for _, existing := range s.state.Cues {
		if existing.Pattern == created.Pattern && existing.Prompt == created.Prompt {
			created = existing
			break
		}
	}
	if !containsCue(s.state.Cues, created.ID) {
		s.state.Cues = append(s.state.Cues, created)
	}
	s.state.Candidates = append(s.state.Candidates[:index], s.state.Candidates[index+1:]...)
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			return Cue{}, false, err
		}
		return created, true, err
	}
	return created, true, nil
}

func (s *Store) uniqueCueNameLocked(preferred, candidateID string) string {
	preferred = boundedCueName(preferred)
	for _, existing := range s.state.Cues {
		if existing.Name != preferred {
			continue
		}
		suffix := "-" + candidateID[:4]
		base := strings.TrimRight(preferred[:min(len(preferred), 32-len(suffix))], "-")
		return base + suffix
	}
	return preferred
}

func (s *Store) Reject(id string, chatID int64, messageID int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.candidateIndexLocked(id, chatID, messageID)
	if index < 0 {
		return false, nil
	}
	previous := clonePersistedState(s.state)
	candidate := s.state.Candidates[index]
	s.state.Candidates = append(s.state.Candidates[:index], s.state.Candidates[index+1:]...)
	s.state.Suppressed = append(s.state.Suppressed, id, "prompt:"+promptHash(candidate.Prompt))
	for _, variant := range candidate.Variants {
		s.state.Suppressed = append(s.state.Suppressed, "prompt:"+promptHash(variant))
	}
	s.pruneLocked()
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			return false, err
		}
		return true, err
	}
	return true, nil
}

func (s *Store) Forget(nameOrID string) (Cue, bool, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, existing := range s.state.Cues {
		if existing.Name != nameOrID && existing.ID != nameOrID && !strings.HasPrefix(existing.ID, nameOrID) {
			continue
		}
		previous := clonePersistedState(s.state)
		s.state.Cues = append(s.state.Cues[:index], s.state.Cues[index+1:]...)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return Cue{}, false, err
			}
			return existing, true, err
		}
		return existing, true, nil
	}
	return Cue{}, false, nil
}

func (s *Store) Matches(context Context, limit int) []Match {
	if limit <= 0 {
		return nil
	}
	s.mu.Lock()
	cues := append([]Cue(nil), s.state.Cues...)
	s.mu.Unlock()
	target := context.Text + "\nprogram: " + context.Program + "\ncwd: " + context.CWD
	matches := make([]Match, 0, min(limit, len(cues)))
	for _, cue := range cues {
		expression, err := regexp.Compile(cue.Pattern)
		if err != nil {
			continue
		}
		indices := expression.FindStringIndex(target)
		if indices == nil {
			continue
		}
		matched := target[indices[0]:indices[1]]
		matches = append(matches, Match{
			CueID: cue.ID, Name: cue.Name, Prompt: cue.Prompt,
			MatchHash: digest(cue.ID + "\x00" + matched), MatchedText: matched,
		})
		if len(matches) == limit {
			break
		}
	}
	return matches
}

func (s *Store) RecordUse(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := clonePersistedState(s.state)
	for index := range s.state.Cues {
		if s.state.Cues[index].ID == id {
			s.state.Cues[index].UseCount++
			if err := s.saveLocked(); err != nil {
				if !PersistenceReachedReplacement(err) {
					s.state = previous
				}
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("cue not found")
}

func associationCounts(observations []Observation, promptHash, featureHash string) (support, promptUses, featureUses int) {
	for _, observation := range observations {
		promptMatch := observation.PromptHash == promptHash
		featureMatch := containsString(observation.FeatureHashes, featureHash)
		if promptMatch {
			promptUses++
		}
		if featureMatch {
			featureUses++
		}
		if promptMatch && featureMatch {
			support++
		}
	}
	return support, promptUses, featureUses
}

func (s *Store) intentClusterLocked(signature intentSignature) []memoryObservation {
	cluster := make([]memoryObservation, 0, len(s.memory))
	for _, observation := range s.memory {
		if intentSimilarity(signature, observation.signature) >= IntentSimilarityThreshold {
			cluster = append(cluster, observation)
		}
	}
	return cluster
}

func intentAssociation(cluster []memoryObservation, featureHash string) (support, uses int, safeVariants []string) {
	seenSafe := make(map[string]bool)
	for _, observation := range cluster {
		uses++
		if !containsString(observation.featureHashes, featureHash) {
			continue
		}
		support++
		if observation.replaySafe && !seenSafe[observation.prompt] {
			seenSafe[observation.prompt] = true
			safeVariants = append(safeVariants, observation.prompt)
		}
	}
	sort.SliceStable(safeVariants, func(i, j int) bool {
		if len(safeVariants[i]) != len(safeVariants[j]) {
			return len(safeVariants[i]) < len(safeVariants[j])
		}
		return safeVariants[i] < safeVariants[j]
	})
	if len(safeVariants) > MaxCandidateVariants {
		safeVariants = safeVariants[:MaxCandidateVariants]
	}
	return support, uses, safeVariants
}

func representativePrompt(variants []string) string {
	if len(variants) == 0 {
		return ""
	}
	return variants[0]
}

func (s *Store) knownLocked(id, pattern, prompt string) bool {
	if containsString(s.state.Suppressed, id) || containsString(s.state.Suppressed, "prompt:"+promptHash(prompt)) {
		return true
	}
	for _, candidate := range s.state.Candidates {
		if candidate.ID == id || candidate.Pattern == pattern && intentSimilarity(makeIntentSignature(candidate.Prompt), makeIntentSignature(prompt)) >= IntentSimilarityThreshold {
			return true
		}
	}
	for _, cue := range s.state.Cues {
		if cue.Pattern == pattern && intentSimilarity(makeIntentSignature(cue.Prompt), makeIntentSignature(prompt)) >= IntentSimilarityThreshold {
			return true
		}
	}
	return false
}

func (s *Store) candidateIndexLocked(id string, chatID int64, messageID int) int {
	for index, candidate := range s.state.Candidates {
		if candidate.ID == id && candidate.ProposalChatID == chatID && candidate.ProposalMessageID == messageID {
			return index
		}
	}
	return -1
}

func (s *Store) candidateByIDLocked(id string) int {
	for index, candidate := range s.state.Candidates {
		if candidate.ID == id {
			return index
		}
	}
	return -1
}

func (s *Store) pruneLocked() {
	if len(s.state.Observations) > MaxObservations {
		s.state.Observations = append([]Observation(nil), s.state.Observations[len(s.state.Observations)-MaxObservations:]...)
	}
	if len(s.state.Candidates) > MaxCandidates {
		s.state.Candidates = append([]Candidate(nil), s.state.Candidates[len(s.state.Candidates)-MaxCandidates:]...)
	}
	if len(s.state.Suppressed) > MaxSuppressed {
		s.state.Suppressed = append([]string(nil), s.state.Suppressed[len(s.state.Suppressed)-MaxSuppressed:]...)
	}
	if len(s.memory) > MaxObservations {
		s.memory = append([]memoryObservation(nil), s.memory[len(s.memory)-MaxObservations:]...)
	}
}

func (s *Store) saveLocked() error {
	s.state.Version = currentVersion
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cue state: %w", err)
	}
	dir := filepath.Dir(s.path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return &persistenceError{err: fmt.Errorf("create cue state temporary file: %w", err)}
	}
	temporary := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return &persistenceError{err: errors.Join(fmt.Errorf("chmod cue state: %w", err), file.Close())}
	}
	if _, err := file.Write(data); err != nil {
		return &persistenceError{err: errors.Join(fmt.Errorf("write cue state: %w", err), file.Close())}
	}
	if err := file.Sync(); err != nil {
		return &persistenceError{err: errors.Join(fmt.Errorf("sync cue state: %w", err), file.Close())}
	}
	if err := file.Close(); err != nil {
		return &persistenceError{err: fmt.Errorf("close cue state: %w", err)}
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return &persistenceError{err: fmt.Errorf("replace cue state: %w", err)}
	}
	keep = true
	if err := syncCueStateDir(dir); err != nil {
		return &persistenceError{err: err, replaced: true}
	}
	return nil
}

func syncCueStateDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open cue state directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		if runtime.GOOS == "darwin" && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)) {
			return nil
		}
		return fmt.Errorf("sync cue state directory: %w", err)
	}
	return nil
}

func validateState(state persistedState) error {
	if len(state.Cues) > MaxActiveCues || len(state.Candidates) > MaxCandidates || len(state.Observations) > MaxObservations || len(state.Suppressed) > MaxSuppressed {
		return fmt.Errorf("cue state exceeds supported bounds")
	}
	for _, cue := range state.Cues {
		if err := validateCueFields(cue.Name, cue.Pattern, cue.Prompt); err != nil {
			return fmt.Errorf("invalid cue %q: %w", cue.ID, err)
		}
	}
	for _, candidate := range state.Candidates {
		if len(candidate.ID) != 16 || len(candidate.Pattern) > MaxPatternBytes || !promptEligible(candidate.Prompt) || len(candidate.Variants) > MaxCandidateVariants {
			return fmt.Errorf("invalid cue candidate")
		}
		if candidate.Name != "" {
			if err := validateCueName(candidate.Name); err != nil {
				return fmt.Errorf("invalid cue candidate name: %w", err)
			}
		}
		for _, variant := range candidate.Variants {
			if !promptEligible(variant) {
				return fmt.Errorf("invalid cue candidate variant")
			}
		}
		if _, err := regexp.Compile(candidate.Pattern); err != nil {
			return fmt.Errorf("invalid cue candidate pattern: %w", err)
		}
	}
	return nil
}

func validateCueFields(name, pattern, prompt string) error {
	if err := validateCueName(name); err != nil {
		return err
	}
	if len(pattern) == 0 || len(pattern) > MaxPatternBytes {
		return fmt.Errorf("cue regex must contain 1 to %d bytes", MaxPatternBytes)
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("compile cue regex: %w", err)
	}
	if !promptEligible(prompt) {
		return fmt.Errorf("cue prompt must contain 8 to %d bytes and cannot be a voice marker or Markdown fence", MaxPromptBytes)
	}
	return nil
}

func validateCueName(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("cue name must contain 1 to 32 characters")
	}
	for _, r := range name {
		if !unicodeCueName(r) {
			return fmt.Errorf("cue name may contain lowercase letters, digits, '-' and '_'")
		}
	}
	return nil
}

func unicodeCueName(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

func containsCue(cues []Cue, id string) bool {
	for _, cue := range cues {
		if cue.ID == id {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

func clonePersistedState(in persistedState) persistedState {
	out := in
	out.Cues = append([]Cue(nil), in.Cues...)
	out.Candidates = cloneCandidates(in.Candidates)
	out.Observations = append([]Observation(nil), in.Observations...)
	for index := range out.Observations {
		out.Observations[index].FeatureHashes = append([]string(nil), in.Observations[index].FeatureHashes...)
	}
	out.Suppressed = append([]string(nil), in.Suppressed...)
	return out
}

func cloneCandidates(in []Candidate) []Candidate {
	out := append([]Candidate(nil), in...)
	for index := range out {
		out[index].Variants = append([]string(nil), in[index].Variants...)
	}
	return out
}

func SortedSnapshot(snapshot Snapshot) Snapshot {
	sort.Slice(snapshot.Cues, func(i, j int) bool { return snapshot.Cues[i].Name < snapshot.Cues[j].Name })
	sort.Slice(snapshot.Candidates, func(i, j int) bool { return snapshot.Candidates[i].CreatedAt.Before(snapshot.Candidates[j].CreatedAt) })
	return snapshot
}
