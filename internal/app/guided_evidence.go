package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const guidedEvidenceContextRows = 2
const guidedEvidenceMaxRows = 18
const guidedEvidenceMinExcerptBytes = 12

type guidedEvidenceCrop struct {
	input terminalshot.Input
	plain string
	hash  string
}

func (a *App) updateGuidedEvidence(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture, excerpts []string) {
	if !a.snapshotReady || a.Snapshots == nil || a.snapshotAnchors() {
		return
	}
	safeExcerpts := excerpts[:0]
	for _, excerpt := range excerpts {
		if a.redactText(excerpt) == excerpt {
			safeExcerpts = append(safeExcerpts, excerpt)
		}
	}
	crop, ok := buildGuidedEvidenceCrop(expected, capture, safeExcerpts, a.Config.SnapshotTheme)
	if !ok || a.redactText(crop.plain) != crop.plain {
		a.retireGuidedEvidence(ctx, expected, "unverified")
		return
	}
	latest, ok := a.Store.FindSession(expected.ID)
	if !ok || latest.State != state.TerminalRunning || !sameTerminalBinding(latest, expected) || latest.AnchorMessageID == 0 {
		return
	}
	if latest.EvidenceMessageID != 0 && latest.EvidenceAnchorMessageID == latest.AnchorMessageID && latest.LastEvidenceHash == crop.hash {
		return
	}
	if !acquireSlot(ctx, a.renderSlots) {
		a.retireGuidedEvidence(ctx, expected, "render_unavailable")
		return
	}
	renderCtx, cancel := context.WithTimeout(ctx, snapshotRenderTimeout)
	pngPath, renderErr := a.Snapshots.Render(renderCtx, crop.input, a.Config.ArtifactDir())
	cancel()
	releaseSlot(a.renderSlots)
	if renderErr != nil {
		_ = a.audit("terminal.guided_evidence", "render_failed", map[string]any{"session_id": expected.ID, "error": renderErr.Error()})
		a.retireGuidedEvidence(ctx, expected, "render_failed")
		return
	}
	defer os.Remove(pngPath)

	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, expected.ID)
	latest, ok = a.Store.FindSession(expected.ID)
	if a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !sameTerminalBinding(latest, expected) || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 {
		_ = a.audit("terminal.guided_evidence", "superseded", map[string]any{"session_id": expected.ID})
		return
	}
	caption := fmt.Sprintf("[%d] highlighted evidence from the current terminal frame", latest.ID)
	if latest.EvidenceMessageID != 0 && latest.EvidenceAnchorMessageID == latest.AnchorMessageID {
		_, err := a.Telegram.EditHTMLPhoto(ctx, latest.AnchorChatID, latest.EvidenceMessageID, pngPath, caption, nil)
		if telegram.IsMessageNotModified(err) {
			err = nil
		}
		if err == nil {
			updated := false
			if _, _, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
				if session.EvidenceMessageID == latest.EvidenceMessageID && session.EvidenceAnchorMessageID == latest.AnchorMessageID && sameTerminalBinding(*session, latest) {
					session.LastEvidenceHash = crop.hash
					updated = true
				}
			}); stateErr != nil {
				_ = a.audit("state.guided_evidence", "failed", map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
				return
			}
			if updated {
				_ = a.audit("terminal.guided_evidence", "updated", map[string]any{"session_id": latest.ID, "rows": crop.input.BufferRows})
			}
			return
		}
		if !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.guided_evidence", "edit_failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
			a.retireGuidedEvidenceLocked(ctx, latest, "edit_failed")
			return
		}
	}
	a.publishGuidedEvidenceLocked(ctx, latest, crop, pngPath, caption)
}

func (a *App) publishGuidedEvidenceLocked(ctx context.Context, latest state.TerminalSession, crop guidedEvidenceCrop, pngPath, caption string) {
	message, err := a.Telegram.SendPhoto(ctx, latest.AnchorChatID, pngPath, caption, latest.AnchorMessageID)
	if err != nil {
		_ = a.audit("telegram.guided_evidence", "send_failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		return
	}
	oldID := latest.EvidenceMessageID
	updated := false
	_, found, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if !a.snapshotAnchors() && session.State == state.TerminalRunning && session.AnchorMessageID == latest.AnchorMessageID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, latest) {
			recordAlternateMessage(session, "evidence", message.MessageID)
			session.EvidenceAnchorMessageID = latest.AnchorMessageID
			session.LastEvidenceHash = crop.hash
			updated = true
		}
	})
	committed := found && updated && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if stateErr != nil {
		outcome := "failed"
		if committed {
			outcome = "durability_uncertain"
		}
		_ = a.audit("state.guided_evidence", outcome, map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
	}
	if !committed {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
		_ = a.Telegram.DeleteMessage(cleanupCtx, message.Chat.ID, message.MessageID)
		cancel()
		return
	}
	if oldID != 0 && oldID != message.MessageID {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
		if err := a.Telegram.DeleteMessage(cleanupCtx, latest.AnchorChatID, oldID); err != nil && !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.guided_evidence", "retire_failed", map[string]any{"session_id": latest.ID, "message_id": oldID, "error": err.Error()})
		}
		cancel()
	}
	_ = a.audit("terminal.guided_evidence", "sent", map[string]any{"session_id": latest.ID, "rows": crop.input.BufferRows})
}

func (a *App) retireGuidedEvidence(ctx context.Context, expected state.TerminalSession, reason string) {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	latest, ok := a.Store.FindSession(expected.ID)
	if !ok || !sameTerminalBinding(latest, expected) {
		return
	}
	a.retireGuidedEvidenceLocked(ctx, latest, reason)
}

func (a *App) retireGuidedEvidenceLocked(ctx context.Context, latest state.TerminalSession, reason string) {
	messageID := latest.EvidenceMessageID
	if messageID == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
	err := a.Telegram.DeleteMessage(cleanupCtx, latest.AnchorChatID, messageID)
	cancel()
	if err != nil && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.guided_evidence", "retire_failed", map[string]any{"session_id": latest.ID, "message_id": messageID, "error": err.Error()})
	}
	if _, _, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if session.EvidenceMessageID == messageID {
			session.EvidenceMessageID = 0
			session.EvidenceAnchorMessageID = 0
			session.LastEvidenceHash = ""
			recordStaleMessage(session, messageID)
		}
	}); stateErr != nil {
		_ = a.audit("state.guided_evidence", "retire_failed", map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
		return
	}
	_ = a.audit("terminal.guided_evidence", "retired", map[string]any{"session_id": latest.ID, "reason": reason})
}

func buildGuidedEvidenceCrop(session state.TerminalSession, capture tmux.StyledCapture, excerpts []string, theme string) (guidedEvidenceCrop, bool) {
	plainRows := captureRows(capture.Text)
	ansiRows := captureRows(capture.ANSI)
	if len(plainRows) == 0 || len(plainRows) != len(ansiRows) {
		return guidedEvidenceCrop{}, false
	}
	selected := make(map[int]bool)
	for _, excerpt := range excerpts {
		rows, ok := matchEvidenceRows(plainRows, excerpt)
		if !ok {
			continue
		}
		for row := rows[0]; row <= rows[1]; row++ {
			selected[row] = true
		}
	}
	if len(selected) == 0 {
		return guidedEvidenceCrop{}, false
	}
	indices := make([]int, 0, len(selected))
	for row := range selected {
		indices = append(indices, row)
	}
	sort.Ints(indices)
	first, last := indices[0], indices[len(indices)-1]
	if last-first+1 > guidedEvidenceMaxRows {
		return guidedEvidenceCrop{}, false
	}
	start := max(0, first-guidedEvidenceContextRows)
	end := min(len(plainRows)-1, last+guidedEvidenceContextRows)
	for end-start+1 > guidedEvidenceMaxRows {
		if first-start > end-last {
			start++
		} else {
			end--
		}
	}
	highlights := make([]int, 0, len(indices))
	for _, row := range indices {
		highlights = append(highlights, row-start)
	}
	input := terminalshot.Input{
		ANSI:          strings.Join(ansiRows[start:end+1], "\n"),
		Title:         firstNonEmpty(session.Title, capture.Title),
		Target:        fmt.Sprintf("[%d]", session.ID),
		CWD:           capture.CurrentPath,
		Columns:       capture.Columns,
		VisibleRows:   capture.VisibleRows,
		BufferRows:    end - start + 1,
		Compact:       true,
		HighlightRows: highlights,
	}
	hash := sha(strings.Join([]string{input.ANSI, fmt.Sprint(highlights), input.Title, input.CWD, theme}, "\x00"))
	return guidedEvidenceCrop{input: input, plain: strings.Join(plainRows[start:end+1], "\n"), hash: hash}, true
}

func matchEvidenceRows(rows []string, excerpt string) ([2]int, bool) {
	needle := strings.Join(strings.Fields(excerpt), " ")
	if len(needle) < guidedEvidenceMinExcerptBytes || len(strings.Fields(needle)) < 2 {
		return [2]int{}, false
	}
	normalized := make([]string, len(rows))
	starts := make([]int, len(rows))
	ends := make([]int, len(rows))
	var flat strings.Builder
	for i, row := range rows {
		normalized[i] = strings.Join(strings.Fields(row), " ")
		if i > 0 {
			flat.WriteByte(' ')
		}
		starts[i] = flat.Len()
		flat.WriteString(normalized[i])
		ends[i] = flat.Len()
	}
	haystack := flat.String()
	match := strings.Index(haystack, needle)
	if match < 0 || strings.Index(haystack[match+1:], needle) >= 0 {
		return [2]int{}, false
	}
	matchEnd := match + len(needle)
	first, last := -1, -1
	for i := range rows {
		if normalized[i] == "" {
			continue
		}
		if ends[i] > match && starts[i] < matchEnd {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last < first {
		return [2]int{}, false
	}
	return [2]int{first, last}, true
}

func captureRows(text string) []string {
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
}
