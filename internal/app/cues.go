package app

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/cue"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const maxAnchorCueSuggestions = 2
const maxAnchorCueSectionBytes = 360

func (a *App) bindAnchorSuggestions(ts state.TerminalSession, text, program, cwd string) state.TerminalSession {
	if a.Cues == nil || ts.State != state.TerminalRunning {
		setAnchorSuggestions(&ts, nil)
		return ts
	}
	matches := a.Cues.Matches(cue.Context{Text: a.redactText(text), Program: program, CWD: a.redactText(cwd)}, 8)
	a.cueMu.Lock()
	defer a.cueMu.Unlock()
	if a.consumedCueMatches == nil {
		a.consumedCueMatches = map[int]map[string]string{}
	}
	consumed := a.consumedCueMatches[ts.ID]
	if consumed == nil {
		consumed = map[string]string{}
		a.consumedCueMatches[ts.ID] = consumed
	}
	current := make(map[string]string, len(matches))
	for _, match := range matches {
		current[match.CueID] = match.MatchHash
		if previous, ok := consumed[match.CueID]; ok && previous != match.MatchHash {
			delete(consumed, match.CueID)
		}
	}
	for id := range consumed {
		if _, ok := current[id]; !ok {
			delete(consumed, id)
		}
	}
	var suggestions []state.AnchorSuggestion
	sectionBytes := len("suggested:\n```\n\n```")
	for _, match := range matches {
		if consumed[match.CueID] == match.MatchHash || strings.Contains(match.Prompt, "```") {
			continue
		}
		entryBytes := len(fmt.Sprintf("%d. %s\n", len(suggestions)+1, match.Prompt))
		if sectionBytes+entryBytes > maxAnchorCueSectionBytes {
			continue
		}
		suggestions = append(suggestions, state.AnchorSuggestion{CueID: match.CueID, Prompt: match.Prompt, MatchHash: match.MatchHash})
		sectionBytes += entryBytes
		if len(suggestions) == maxAnchorCueSuggestions {
			break
		}
	}
	setAnchorSuggestions(&ts, suggestions)
	return ts
}

func setAnchorSuggestions(ts *state.TerminalSession, suggestions []state.AnchorSuggestion) {
	ts.AnchorSuggestions = append([]state.AnchorSuggestion(nil), suggestions...)
	var identity []string
	for _, suggestion := range suggestions {
		identity = append(identity, suggestion.CueID, suggestion.Prompt, suggestion.MatchHash)
	}
	if len(identity) == 0 {
		ts.AnchorSuggestionToken = ""
		return
	}
	ts.AnchorSuggestionToken = sha(strings.Join(identity, "\x00"))[:16]
}

func renderAnchorSuggestions(suggestions []state.AnchorSuggestion) string {
	if len(suggestions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("suggested:\n```\n")
	for index, suggestion := range suggestions {
		fmt.Fprintf(&b, "%d. %s\n", index+1, suggestion.Prompt)
	}
	b.WriteString("```")
	return b.String()
}

func appendAnchorSuggestions(text string, suggestions []state.AnchorSuggestion) string {
	section := renderAnchorSuggestions(suggestions)
	if section == "" {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + section
}

func resolveAnchorSuggestion(ts state.TerminalSession, token string, index int) (state.AnchorSuggestion, bool) {
	if ts.State != state.TerminalRunning || token == "" || token != ts.AnchorSuggestionToken || index <= 0 || index > len(ts.AnchorSuggestions) {
		return state.AnchorSuggestion{}, false
	}
	return ts.AnchorSuggestions[index-1], true
}

func (a *App) consumeCueMatch(sessionID int, suggestion state.AnchorSuggestion) {
	a.cueMu.Lock()
	defer a.cueMu.Unlock()
	if a.consumedCueMatches == nil {
		a.consumedCueMatches = map[int]map[string]string{}
	}
	if a.consumedCueMatches[sessionID] == nil {
		a.consumedCueMatches[sessionID] = map[string]string{}
	}
	a.consumedCueMatches[sessionID][suggestion.CueID] = suggestion.MatchHash
}

func (a *App) cueMatchConsumed(sessionID int, suggestion state.AnchorSuggestion) bool {
	a.cueMu.Lock()
	defer a.cueMu.Unlock()
	return a.consumedCueMatches != nil && a.consumedCueMatches[sessionID] != nil && a.consumedCueMatches[sessionID][suggestion.CueID] == suggestion.MatchHash
}

func (a *App) observeCueReply(ctx context.Context, msg telegram.Message, session state.TerminalSession, frame snapshotTextFrame, text string) {
	if a.Cues == nil || frame.MessageID == 0 {
		return
	}
	candidate, err := a.Cues.Observe(cue.Context{
		Text: a.redactText(frame.JoinedText), Program: "", CWD: a.redactText(session.LastKnownCWD),
	}, a.redactText(text), time.Now().UTC())
	if err != nil {
		outcome := "failed"
		if cue.PersistenceReachedReplacement(err) {
			outcome = "durability_uncertain"
		}
		_ = a.audit("cue.observe", outcome, map[string]any{"session_id": session.ID, "error": err.Error()})
		if candidate == nil {
			return
		}
	}
	if candidate == nil {
		return
	}
	proposal := cueProposalHTML(*candidate)
	response, err := a.Telegram.SendHTMLMessage(ctx, msg.Chat.ID, proposal, msg.MessageID, telegram.CueProposalMarkup(candidate.ID))
	if err != nil {
		_ = a.audit("cue.proposal", "send_failed", map[string]any{"candidate_id": candidate.ID, "error": err.Error()})
		return
	}
	if err := a.Cues.BindProposal(candidate.ID, response.Chat.ID, response.MessageID); err != nil {
		if !cue.PersistenceReachedReplacement(err) {
			_ = a.Telegram.DeleteMessage(ctx, response.Chat.ID, response.MessageID)
			_ = a.audit("cue.proposal", "bind_failed", map[string]any{"candidate_id": candidate.ID, "error": err.Error()})
			return
		}
		_ = a.audit("cue.proposal", "durability_uncertain", map[string]any{"candidate_id": candidate.ID, "error": err.Error()})
	}
	_ = a.audit("cue.proposal", "sent", map[string]any{"candidate_id": candidate.ID, "support": candidate.Support, "confidence": candidate.ConfidencePercent})
}

func cueProposalHTML(candidate cue.Candidate) string {
	return fmt.Sprintf("Possible cue\n\nWhen:\n<pre>%s</pre>\n\nSuggest:\n<pre>%s</pre>\n\nObserved together %d times (%d%% association). Nothing will use this cue unless you save it.",
		html.EscapeString(candidate.Pattern), html.EscapeString(candidate.Prompt), candidate.Support, candidate.ConfidencePercent)
}

func (a *App) retireCueProposal(ctx context.Context, chatID int64, messageID int) {
	if err := a.Telegram.DeleteMessage(ctx, chatID, messageID); err == nil || isTelegramMessageGone(err) {
		return
	}
	if _, err := a.Telegram.EditReplyMarkup(ctx, chatID, messageID, telegram.ClearMarkup()); err != nil && !telegram.IsMessageNotModified(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.cue_proposal", "retire_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}

func (a *App) handleCuesCommand(ctx context.Context, msg telegram.Message, args string) actionResult {
	if a.Cues == nil {
		return actionResult{Outcome: actionUserError, Message: "Cues are disabled. Set ENGRAM_CUES=on and restart Engram to enable local cue learning."}
	}
	args = strings.TrimSpace(args)
	if args == "" {
		a.reply(ctx, msg, cueListText(a.Cues.Snapshot()))
		return actionResult{Outcome: actionOK, Message: "listed cues"}
	}
	if strings.HasPrefix(args, "forget ") {
		name := strings.TrimSpace(strings.TrimPrefix(args, "forget "))
		removed, found, err := a.Cues.Forget(name)
		if err != nil && !found {
			return actionResult{Outcome: actionStateFailed, Message: err.Error()}
		}
		if !found {
			return actionResult{Outcome: actionUserError, Message: "cue not found"}
		}
		if err != nil {
			_ = a.audit("cue.forget", "durability_uncertain", map[string]any{"cue_id": removed.ID, "error": err.Error()})
		}
		a.reply(ctx, msg, "Forgot cue "+removed.Name+".")
		return actionResult{Outcome: actionOK, Message: "forgot cue"}
	}
	if strings.HasPrefix(args, "save ") {
		lines := strings.Split(args, "\n")
		first := strings.Fields(lines[0])
		if len(first) != 2 || len(lines) < 3 {
			return actionResult{Outcome: actionUserError, Message: "usage: /cues save <name>\\n<regex>\\n<prompt>"}
		}
		pattern := strings.TrimSpace(lines[1])
		prompt := strings.TrimSpace(strings.Join(lines[2:], "\n"))
		created, err := a.Cues.Add(first[1], pattern, a.redactText(prompt), time.Now().UTC())
		if err != nil {
			if created.ID != "" && cue.PersistenceReachedReplacement(err) {
				_ = a.audit("cue.save", "durability_uncertain", map[string]any{"cue_id": created.ID, "error": err.Error()})
				a.reply(ctx, msg, "Saved cue "+created.Name+".")
				return actionResult{Outcome: actionOK, Message: "saved cue"}
			}
			return actionResult{Outcome: actionUserError, Message: err.Error()}
		}
		a.reply(ctx, msg, "Saved cue "+created.Name+".")
		return actionResult{Outcome: actionOK, Message: "saved cue"}
	}
	return actionResult{Outcome: actionUserError, Message: "usage: /cues [save <name>\\n<regex>\\n<prompt> | forget <name>]"}
}

func cueListText(snapshot cue.Snapshot) string {
	snapshot = cue.SortedSnapshot(snapshot)
	if len(snapshot.Cues) == 0 && len(snapshot.Candidates) == 0 {
		return "Cues\n\nNo active or proposed cues."
	}
	var b strings.Builder
	b.WriteString("Cues")
	if len(snapshot.Cues) > 0 {
		b.WriteString("\n\nActive")
		for _, item := range snapshot.Cues {
			fmt.Fprintf(&b, "\n%s (%d uses)\nwhen: %s\nsuggest: %s\n", item.Name, item.UseCount, item.Pattern, item.Prompt)
		}
	}
	if len(snapshot.Candidates) > 0 {
		b.WriteString("\n\nAwaiting a proposal")
		for _, item := range snapshot.Candidates {
			fmt.Fprintf(&b, "\n%s: %s -> %s", item.ID[:6], item.Pattern, item.Prompt)
		}
	}
	return strings.TrimSpace(b.String())
}
