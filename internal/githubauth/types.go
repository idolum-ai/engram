package githubauth

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	ProtocolVersion = 2
	ActionExec      = "exec"
	ActionGrant     = "grant"
	ActionStatus    = "status"
	ActionRevoke    = "revoke"
	MaxRequestBytes = 64 << 10

	ErrorCodeLocalPassphraseRequired = "local_passphrase_required"
	MinGrantDuration                 = 15 * time.Minute
	AbsoluteMaxGrantDuration         = 24 * time.Hour
)

type Binding struct {
	ServerID string `json:"server_id"`
	WindowID string `json:"window_id"`
	PaneID   string `json:"pane_id"`
}

type BrokerRequest struct {
	Version      int               `json:"version"`
	Action       string            `json:"action"`
	App          string            `json:"app,omitempty"`
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Binding      Binding           `json:"binding"`
	Passphrase   []byte            `json:"passphrase,omitempty"`
	LocalUnlock  bool              `json:"local_unlock,omitempty"`
	GrantFor     time.Duration     `json:"grant_for,omitempty"`
	Purpose      string            `json:"purpose,omitempty"`
}

type LeaseInfo struct {
	App          string            `json:"app"`
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
	ExpiresAt    time.Time         `json:"expires_at"`
	GrantID      string            `json:"grant_id,omitempty"`
	Generation   uint64            `json:"generation,omitempty"`
}

type GrantInfo struct {
	ID           string            `json:"id"`
	App          string            `json:"app"`
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
	Purpose      string            `json:"purpose"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
}

type BrokerResponse struct {
	OK              bool        `json:"ok"`
	Error           string      `json:"error,omitempty"`
	ErrorCode       string      `json:"error_code,omitempty"`
	Token           string      `json:"token,omitempty"`
	ExpiresAt       time.Time   `json:"expires_at,omitempty"`
	Leases          []LeaseInfo `json:"leases,omitempty"`
	Grants          []GrantInfo `json:"grants,omitempty"`
	DeliveryPending bool        `json:"delivery_pending,omitempty"`
}

func (r *BrokerRequest) Normalize() {
	r.App = strings.TrimSpace(r.App)
	r.Action = strings.TrimSpace(r.Action)
	r.Repositories = normalizeRepositories(r.Repositories)
	normalizedPermissions := make(map[string]string, len(r.Permissions))
	for name, level := range r.Permissions {
		normalizedPermissions[strings.TrimSpace(name)] = strings.ToLower(strings.TrimSpace(level))
	}
	r.Permissions = normalizedPermissions
}

func (r BrokerRequest) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported GitHub broker protocol version")
	}
	if r.Binding.ServerID == "" || r.Binding.WindowID == "" || r.Binding.PaneID == "" {
		return fmt.Errorf("missing tmux terminal binding")
	}
	switch r.Action {
	case ActionStatus, ActionRevoke:
		return nil
	case ActionExec, ActionGrant:
	default:
		return fmt.Errorf("unknown GitHub broker action")
	}
	if err := validateAlias(r.App); err != nil {
		return err
	}
	if len(r.Repositories) == 0 {
		return fmt.Errorf("at least one explicit repository is required")
	}
	if len(r.Repositories) > 100 {
		return fmt.Errorf("at most 100 repositories may be requested")
	}
	for _, repository := range r.Repositories {
		if err := validateRepository(repository); err != nil {
			return err
		}
	}
	if len(r.Permissions) == 0 {
		return fmt.Errorf("at least one explicit permission is required")
	}
	if len(r.Permissions) > 100 {
		return fmt.Errorf("at most 100 permissions may be requested")
	}
	for name, level := range r.Permissions {
		if err := validatePermission(name, level); err != nil {
			return err
		}
	}
	if r.Action == ActionGrant {
		if r.GrantFor < MinGrantDuration || r.GrantFor > AbsoluteMaxGrantDuration {
			return fmt.Errorf("renewable grant duration must be between %s and %s", MinGrantDuration, AbsoluteMaxGrantDuration)
		}
		if len(r.Purpose) == 0 || len(r.Purpose) > 200 || strings.TrimSpace(r.Purpose) == "" {
			return fmt.Errorf("renewable grant purpose must be between 1 and 200 bytes")
		}
		for _, character := range r.Purpose {
			if unicode.Is(unicode.Cc, character) || unicode.Is(unicode.Cf, character) ||
				unicode.Is(unicode.Zl, character) || unicode.Is(unicode.Zp, character) {
				return fmt.Errorf("renewable grant purpose contains an unsafe Unicode control")
			}
		}
		if err := ValidateRenewablePermissions(r.Permissions); err != nil {
			return err
		}
		if len(r.Command) != 0 {
			return fmt.Errorf("renewable grant requests cannot include a child command")
		}
		return nil
	}
	if len(r.Command) == 0 || strings.TrimSpace(r.Command[0]) == "" {
		return fmt.Errorf("a child command is required")
	}
	if len(r.Command) > 256 {
		return fmt.Errorf("child command has too many arguments")
	}
	total := 0
	for _, argument := range r.Command {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("child command contains a NUL byte")
		}
		total += len(argument)
	}
	if total > 32*1024 {
		return fmt.Errorf("child command exceeds 32768 bytes")
	}
	return nil
}

// ValidateRenewablePermissions fails closed for renewable write authority.
// Evolving read-only permissions remain eligible, while writes are limited to
// collaboration surfaces whose unattended risk is explicit and reviewable.
func ValidateRenewablePermissions(permissions map[string]string) error {
	allowedWrites := map[string]bool{
		"checks":              true,
		"contents":            true,
		"discussions":         true,
		"issues":              true,
		"pull_requests":       true,
		"repository_projects": true,
		"statuses":            true,
	}
	for name, level := range permissions {
		if level == "write" && !allowedWrites[name] {
			return fmt.Errorf("permission %s=write is not on the renewable collaboration allowlist; use exact-command approval", name)
		}
	}
	return nil
}

func validateRepository(repository string) error {
	if len(repository) == 0 || len(repository) > 200 || strings.Count(repository, "/") != 1 {
		return fmt.Errorf("repository %q must use owner/name form", repository)
	}
	parts := strings.Split(repository, "/")
	for _, part := range parts {
		if len(part) == 0 || part == "." || part == ".." {
			return fmt.Errorf("repository %q must use owner/name form", repository)
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return fmt.Errorf("repository %q contains unsupported characters", repository)
		}
	}
	return nil
}

func validatePermission(name, level string) error {
	if len(name) == 0 || len(name) > 100 {
		return fmt.Errorf("invalid GitHub App permission name")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return fmt.Errorf("invalid GitHub App permission %q", name)
	}
	if level != "read" && level != "write" {
		return fmt.Errorf("permission %s must be read or write", name)
	}
	return nil
}

func normalizeRepositories(repositories []string) []string {
	seen := map[string]string{}
	for _, repository := range repositories {
		repository = strings.TrimSpace(repository)
		if repository == "" {
			continue
		}
		key := strings.ToLower(repository)
		if _, exists := seen[key]; !exists {
			seen[key] = repository
		}
	}
	out := make([]string, 0, len(seen))
	for _, repository := range seen {
		out = append(out, repository)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func PermissionsSubset(requested, granted map[string]string) bool {
	for name, requestedLevel := range requested {
		grantedLevel, ok := granted[name]
		if !ok || permissionRank(requestedLevel) > permissionRank(grantedLevel) {
			return false
		}
	}
	return true
}

func RepositoriesSubset(requested, granted []string) bool {
	available := make(map[string]bool, len(granted))
	for _, repository := range granted {
		available[strings.ToLower(repository)] = true
	}
	for _, repository := range requested {
		if !available[strings.ToLower(repository)] {
			return false
		}
	}
	return true
}

func permissionRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	default:
		return 100
	}
}

func CompactLeaseLine(lease LeaseInfo, now time.Time) string {
	if !lease.ExpiresAt.After(now) {
		return ""
	}
	readCount, writeCount := 0, 0
	for _, level := range lease.Permissions {
		switch level {
		case "read":
			readCount++
		case "write":
			writeCount++
		}
	}
	repositoryLabel := fmt.Sprintf("%d repos", len(lease.Repositories))
	if len(lease.Repositories) == 1 {
		repositoryLabel = "1 repo"
	}
	remaining := lease.ExpiresAt.Sub(now).Round(time.Minute)
	if remaining < time.Minute {
		remaining = time.Minute
	}
	authority := fmt.Sprintf("%dR %dW", readCount, writeCount)
	if writeCount == 0 {
		authority = "read-only"
	}
	return fmt.Sprintf("GH %s · %s · %s · %s", lease.App, authority, repositoryLabel, compactDuration(remaining))
}

func CompactGrantLine(grant GrantInfo, now time.Time) string {
	if !grant.ExpiresAt.After(now) {
		return ""
	}
	readCount, writeCount := 0, 0
	for _, level := range grant.Permissions {
		switch level {
		case "read":
			readCount++
		case "write":
			writeCount++
		}
	}
	repositoryLabel := fmt.Sprintf("%d repos", len(grant.Repositories))
	if len(grant.Repositories) == 1 {
		repositoryLabel = "1 repo"
	}
	authority := fmt.Sprintf("%dR %dW", readCount, writeCount)
	if writeCount == 0 {
		authority = "read-only"
	}
	remaining := grant.ExpiresAt.Sub(now).Round(time.Minute)
	if remaining < time.Minute {
		remaining = time.Minute
	}
	return fmt.Sprintf("GH grant %s · %s · %s · %s", grant.App, authority, repositoryLabel, compactDuration(remaining))
}

func compactDuration(duration time.Duration) string {
	if duration < time.Hour {
		return fmt.Sprintf("%dm", max(1, int(duration.Round(time.Minute)/time.Minute)))
	}
	hours := int(duration / time.Hour)
	minutes := int(duration.Round(time.Minute)/time.Minute) % 60
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%02dm", hours, minutes)
}

func clonePermissions(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
