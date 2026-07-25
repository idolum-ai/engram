package claudeui

import (
	"strings"

	"github.com/idolum-ai/engram/internal/agentui"
)

const unknownRuntimeModel = "claude-runtime"

func Analyze(runtime Runtime, observation agentui.Observation, verifiedModel string) agentui.Analysis {
	fallback := agentui.Analysis{
		Original: observation.Current.Text, Conversation: observation.Current.Text,
		Activity: agentui.ActivityUnknown,
	}
	if !runtime.Detected || !runtime.Supported || !supportedVersion(runtime.Version) || runtime.Identity == "" {
		return fallback
	}
	model := verifiedModel
	if model == "" {
		// The process and version are verified, but model identity is not. The
		// sentinel permits activity/chrome parsing without presenting a guessed
		// model to the user.
		model = unknownRuntimeModel
	}
	observation.VerifiedProgram = "claude"
	observation.VerifiedModel = model
	analysis := agentui.Analyze(observation)
	if !analysis.Applied || !strings.HasPrefix(analysis.Model, "claude-") {
		return fallback
	}
	if analysis.Model == unknownRuntimeModel {
		analysis.Model = ""
	}
	return analysis
}
