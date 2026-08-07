// Package agentcompat defines the provider-neutral compatibility and
// presentation contract shared by live capture, diagnostics, and state.
package agentcompat

import (
	"strings"
	"time"
	"unicode/utf8"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
)

const (
	CodexProcessContract     = "codex-process-v1"
	ClaudeProcessContract    = "claude-process-v1"
	CodexBindingContract     = "codex-session-start-v1"
	ClaudeBindingContract    = "claude-session-start-v1"
	CodexScreenContract      = "codex-screen-v1"
	ClaudeScreenContract     = "claude-screen-v1"
	CodexTranscriptContract  = "codex-rollout-v1"
	ClaudeTranscriptContract = "claude-transcript-v1"
)

func ValidProvider(provider Provider) bool {
	return provider == ProviderCodex || provider == ProviderClaude
}

func DisplayName(provider Provider) string {
	if provider == ProviderClaude {
		return "Claude Code"
	}
	if provider == ProviderCodex {
		return "Codex"
	}
	return "Agent"
}

type State string

const (
	StateProven      State = "proven"
	StateUnavailable State = "unavailable"
	StateMissing     State = "missing"
	StateStale       State = "stale"
	StateUnsupported State = "unsupported"
	StateSupported   State = "supported"
	StateLiteral     State = "literal"
	StateDisabled    State = "disabled"
	StateEligible    State = "eligible"
)

type Reason string

const (
	ReasonNone                    Reason = ""
	ReasonProcessNotFound         Reason = "process_not_found"
	ReasonProcessAmbiguous        Reason = "process_ambiguous"
	ReasonProcessIdentityUnproven Reason = "process_identity_unproven"
	ReasonProbeUnavailable        Reason = "probe_unavailable"
	ReasonBindingMissing          Reason = "binding_missing"
	ReasonBindingStale            Reason = "binding_stale"
	ReasonBindingUnsupported      Reason = "binding_unsupported"
	ReasonScreenVersionUnknown    Reason = "screen_version_unknown"
	ReasonScreenLayoutUnknown     Reason = "screen_layout_unknown"
	ReasonContextDisabled         Reason = "context_disabled"
	ReasonTranscriptMissing       Reason = "transcript_missing"
	ReasonTranscriptAmbiguous     Reason = "transcript_ambiguous"
	ReasonTranscriptUnsupported   Reason = "transcript_unsupported"
	ReasonTranscriptEligible      Reason = "transcript_eligible"
	ReasonIdentityChanged         Reason = "identity_changed"
)

type Axis struct {
	State    State  `json:"state"`
	Contract string `json:"contract,omitempty"`
	Version  string `json:"version,omitempty"`
	Reason   Reason `json:"reason,omitempty"`
}

type Compatibility struct {
	Provider   Provider `json:"provider,omitempty"`
	Process    Axis     `json:"process"`
	Binding    Axis     `json:"binding"`
	Screen     Axis     `json:"screen"`
	Transcript Axis     `json:"transcript"`
}

type Provenance string

const (
	ProvenanceHook       Provenance = "hook"
	ProvenanceVisibleUI  Provenance = "visible_ui"
	ProvenanceRetainedUI Provenance = "retained_same_incarnation_ui"
)

type Value struct {
	Value      string     `json:"value,omitempty"`
	Provenance Provenance `json:"provenance,omitempty"`
}

type Presentation struct {
	Model           Value     `json:"model,omitempty"`
	Effort          Value     `json:"effort,omitempty"`
	Interaction     Value     `json:"interaction,omitempty"`
	Activity        string    `json:"activity,omitempty"`
	LastTurnSeconds int       `json:"last_turn_seconds,omitempty"`
	AgentTotal      int       `json:"agent_total,omitempty"`
	AgentActive     int       `json:"agent_active,omitempty"`
	ObservedAt      time.Time `json:"observed_at,omitempty"`
}

type Viewport struct {
	Applied         bool   `json:"applied,omitempty"`
	Contract        string `json:"contract,omitempty"`
	RuntimeIdentity string `json:"runtime_identity,omitempty"`
	TmuxIdentity    string `json:"tmux_identity,omitempty"`
	Boundary        string `json:"boundary,omitempty"`
	StartLine       int    `json:"start_line,omitempty"`
	AlternateScreen string `json:"alternate_screen,omitempty"`
	CopyMode        string `json:"copy_mode,omitempty"`
}

const (
	MaxContractBytes = 64
	MaxVersionBytes  = 32
	MaxValueBytes    = 96
	MaxActivityBytes = 32
	MaxDuration      = 7 * 24 * time.Hour
	MaxAgentCount    = 99
)

func NormalizeCompatibility(value Compatibility) Compatibility {
	if !ValidProvider(value.Provider) {
		value.Provider = ""
	}
	value.Process = normalizeAxis(value.Process)
	value.Binding = normalizeAxis(value.Binding)
	value.Screen = normalizeAxis(value.Screen)
	value.Transcript = normalizeAxis(value.Transcript)
	return value
}

func normalizeAxis(axis Axis) Axis {
	if !validState(axis.State) {
		axis = Axis{}
	}
	axis.Contract = boundToken(axis.Contract, MaxContractBytes)
	axis.Version = boundToken(axis.Version, MaxVersionBytes)
	if !validReason(axis.Reason) {
		axis.Reason = ReasonProbeUnavailable
	}
	return axis
}

func NormalizePresentation(value Presentation) Presentation {
	value.Model = normalizeValue(value.Model, MaxValueBytes)
	value.Effort = normalizeValue(value.Effort, 16)
	value.Interaction = normalizeValue(value.Interaction, 24)
	value.Activity = boundToken(value.Activity, MaxActivityBytes)
	if value.LastTurnSeconds < 0 || time.Duration(value.LastTurnSeconds)*time.Second > MaxDuration {
		value.LastTurnSeconds = 0
	}
	if value.AgentTotal < 0 || value.AgentTotal > MaxAgentCount {
		value.AgentTotal = 0
	}
	if value.AgentActive < 0 || value.AgentActive > value.AgentTotal {
		value.AgentActive = 0
	}
	return value
}

func NormalizeViewport(value Viewport) Viewport {
	if !value.Applied {
		return Viewport{}
	}
	value.Contract = boundToken(value.Contract, MaxContractBytes)
	value.RuntimeIdentity = boundHex(value.RuntimeIdentity, 64)
	value.TmuxIdentity = boundHex(value.TmuxIdentity, 64)
	value.Boundary = boundToken(value.Boundary, 32)
	if value.Contract == "" || value.RuntimeIdentity == "" || value.TmuxIdentity == "" || value.Boundary == "" || value.StartLine < 0 || value.StartLine > 400 {
		return Viewport{}
	}
	if value.AlternateScreen != "0" && value.AlternateScreen != "1" && value.AlternateScreen != "off" && value.AlternateScreen != "on" {
		value.AlternateScreen = ""
	}
	if value.CopyMode != "0" && value.CopyMode != "1" && value.CopyMode != "off" && value.CopyMode != "on" {
		value.CopyMode = ""
	}
	return value
}

func normalizeValue(value Value, maximum int) Value {
	value.Value = boundToken(value.Value, maximum)
	if value.Value == "" {
		return Value{}
	}
	switch value.Provenance {
	case ProvenanceHook, ProvenanceVisibleUI, ProvenanceRetainedUI:
	default:
		return Value{}
	}
	return value
}

func validState(value State) bool {
	switch value {
	case StateProven, StateUnavailable, StateMissing, StateStale, StateUnsupported, StateSupported, StateLiteral, StateDisabled, StateEligible:
		return true
	default:
		return false
	}
}

func validReason(value Reason) bool {
	switch value {
	case ReasonNone, ReasonProcessNotFound, ReasonProcessAmbiguous, ReasonProcessIdentityUnproven, ReasonProbeUnavailable,
		ReasonBindingMissing, ReasonBindingStale, ReasonBindingUnsupported, ReasonScreenVersionUnknown, ReasonScreenLayoutUnknown,
		ReasonContextDisabled, ReasonTranscriptMissing, ReasonTranscriptAmbiguous, ReasonTranscriptUnsupported,
		ReasonTranscriptEligible, ReasonIdentityChanged:
		return true
	default:
		return false
	}
}

func boundToken(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	if len(value) > maximum {
		value = value[:maximum]
		for value != "" && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func boundHex(value string, maximum int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > maximum {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' && r < 'a' || r > 'f' {
			return ""
		}
	}
	return value
}
