package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const guidedEvidenceContextRows = 2
const guidedEvidenceMaxRows = 18
const guidedEvidenceMinExcerptBytes = 12
const guidedCaptionBytes = 960
const guidedTailRows = 10

const (
	guidedEvidenceVerified = "verified"
	guidedEvidenceRecent   = "recent_activity"
	guidedEvidenceTail     = "terminal_tail"
	guidedEvidenceNone     = "no_evidence"
)

type guidedEvidenceCrop struct {
	input  terminalshot.Input
	plain  string
	hash   string
	source string
}

func (a *App) updateGuidedAnchorWithEvidence(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture, previous conversationFrame, summary string, refs visibleReferences, excerpts []string, force bool, guard, accepted func() bool) bool {
	if !a.snapshotReady || a.Snapshots == nil || a.snapshotAnchors() {
		return false
	}
	crop := a.selectGuidedEvidenceCrop(expected, capture, previous, excerpts)
	if !acquireSlot(ctx, a.renderSlots) {
		return false
	}
	renderCtx, cancel := context.WithTimeout(ctx, snapshotRenderTimeout)
	pngPath, renderErr := a.Snapshots.Render(renderCtx, crop.input, a.Config.ArtifactDir())
	cancel()
	releaseSlot(a.renderSlots)
	if renderErr != nil {
		_ = a.audit("terminal.guided_evidence", "render_failed", map[string]any{"session_id": expected.ID, "error": renderErr.Error()})
		return false
	}
	defer os.Remove(pngPath)

	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, expected.ID)
	latest, ok := a.Store.FindSession(expected.ID)
	if a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !sameTerminalBinding(latest, expected) || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || guard != nil && !guard() {
		_ = a.audit("terminal.guided_evidence", "superseded", map[string]any{"session_id": expected.ID})
		return false
	}
	if latest.Title != expected.Title {
		return false
	}
	caption, files := a.guidedEvidenceCaption(latest, summary, refs)
	renderHash := sha(caption + "\x00" + crop.hash)
	finish := func() bool {
		if guard != nil && !guard() {
			return false
		}
		return accepted == nil || accepted()
	}
	if renderHash == latest.LastRenderHash && !force {
		return finish()
	}
	if !force && time.Since(latest.LastAnchorEditAt) < 10*time.Second {
		return false
	}
	presented := bindAnchorFiles(latest, files)
	presented.AnchorFormat = anchorFormatGuideEvidence
	markup := a.anchorMarkup(presented)
	_, editErr := a.Telegram.EditHTMLPhoto(ctx, latest.AnchorChatID, latest.AnchorMessageID, pngPath, telegram.MarkdownToHTML(caption), markup)
	if telegram.IsMessageNotModified(editErr) {
		editErr = nil
	}
	if editErr != nil {
		if telegram.IsRateLimited(editErr) || !isTelegramAnchorUnavailable(editErr) {
			_ = a.audit("telegram.guided_evidence", "edit_failed", map[string]any{"session_id": latest.ID, "error": editErr.Error()})
			return false
		}
		message, sendErr := a.Telegram.SendHTMLPhotoWithMarkup(ctx, latest.AnchorChatID, pngPath, telegram.MarkdownToHTML(caption), 0, markup)
		if sendErr != nil {
			_ = a.audit("telegram.guided_evidence", "replacement_failed", map[string]any{"session_id": latest.ID, "error": sendErr.Error()})
			return false
		}
		if guard != nil && !guard() {
			a.deactivateProspectiveMediaAnchor(ctx, message.Chat.ID, message.MessageID)
			return false
		}
		oldID := latest.AnchorMessageID
		oldFormat := firstNonEmpty(latest.AnchorFormat, anchorFormatText)
		replaced := false
		updated, found, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
			if !a.snapshotAnchors() && session.State == state.TerminalRunning && session.AnchorMessageID == oldID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, latest) {
				session.AnchorChatID = message.Chat.ID
				session.AnchorMessageID = message.MessageID
				session.AnchorFormat = anchorFormatGuideEvidence
				session.RetiringAnchorMessageID = oldID
				session.RetiringAnchorFormat = oldFormat
				session.AnchorPinned = false
				session.AnchorPinKnown = false
				session.LastRenderHash = renderHash
				session.LastAnchorEditAt = time.Now().UTC()
				setAnchorFiles(session, files)
				replaced = true
			}
		})
		committed := found && replaced && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
		if !committed {
			a.deactivateProspectiveMediaAnchor(ctx, message.Chat.ID, message.MessageID)
			return false
		}
		if stateErr != nil {
			_ = a.audit("state.guided_evidence", "durability_uncertain", map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
		}
		if anchorShouldBePinned(updated) {
			a.ensureCurrentAnchorPinnedLocked(ctx, updated)
		}
		a.finishAnchorRotationLocked(ctx, latest.ID)
		_ = a.audit("terminal.guided_evidence", "replaced", map[string]any{"session_id": latest.ID, "source": crop.source, "rows": crop.input.BufferRows})
		return finish()
	}
	if guard != nil && !guard() {
		return false
	}
	updated := false
	if _, _, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if !a.snapshotAnchors() && session.State == state.TerminalRunning && session.AnchorMessageID == latest.AnchorMessageID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, latest) {
			session.AnchorFormat = anchorFormatGuideEvidence
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			setAnchorFiles(session, files)
			updated = true
		}
	}); stateErr != nil {
		_ = a.audit("state.guided_evidence", "failed", map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
		return false
	}
	if !updated {
		return false
	}
	_ = a.audit("terminal.guided_evidence", "updated", map[string]any{"session_id": latest.ID, "source": crop.source, "rows": crop.input.BufferRows})
	return finish()
}

func (a *App) guidedEvidenceCaption(session state.TerminalSession, summary string, refs visibleReferences) (string, []string) {
	title := strings.Join(strings.Fields(a.redactText(firstNonEmpty(session.Title, "terminal"))), " ")
	if len(title) > 40 {
		title = headUTF8(title, 40)
	}
	header := fmt.Sprintf("[%d] %s  %s", session.ID, session.State, title)
	if session.LastKnownCWD != "" {
		header += "\ncwd: " + strings.Join(strings.Fields(a.redactText(session.LastKnownCWD)), " ")
	}
	remaining := guidedCaptionBytes - len(header) - 2
	if remaining <= 0 {
		return headUTF8(header, guidedCaptionBytes), nil
	}
	referenceBudget := min(300, remaining/3)
	references, files := renderSnapshotReferenceSetWithFiles(refs, referenceBudget)
	summaryBudget := remaining
	if references != "" {
		summaryBudget -= len(references) + 2
	}
	summary = truncateAtWord(a.redactText(summary), summaryBudget)
	caption := header + "\n\n" + summary
	if references != "" {
		caption += "\n\n" + references
	}
	return caption, files
}

func truncateAtWord(text string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxBytes {
		return text
	}
	if maxBytes <= 3 {
		return headUTF8(text, maxBytes)
	}
	trimmed := headUTF8(text, maxBytes-3)
	if cut := strings.LastIndexAny(trimmed, " \n\t"); cut > maxBytes/2 {
		trimmed = strings.TrimSpace(trimmed[:cut])
	}
	return trimmed + "..."
}

func (a *App) selectGuidedEvidenceCrop(session state.TerminalSession, capture tmux.StyledCapture, previous conversationFrame, excerpts []string) guidedEvidenceCrop {
	safeExcerpts := make([]string, 0, len(excerpts))
	for _, excerpt := range excerpts {
		if a.redactText(excerpt) == excerpt {
			safeExcerpts = append(safeExcerpts, excerpt)
		}
	}
	builders := []func() (guidedEvidenceCrop, bool){
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedEvidenceCrop(session, capture, safeExcerpts, a.Config.SnapshotTheme)
		},
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedRecentActivityCrop(session, capture, previous, a.Config.SnapshotTheme)
		},
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedTailCrop(session, capture, a.Config.SnapshotTheme)
		},
	}
	for _, build := range builders {
		if crop, ok := build(); ok && a.redactText(crop.plain) == crop.plain {
			return crop
		}
	}
	return buildGuidedEvidencePlaceholder(session, capture, a.Config.SnapshotTheme)
}

func buildGuidedEvidencePlaceholder(session state.TerminalSession, capture tmux.StyledCapture, theme string) guidedEvidenceCrop {
	const message = "No verified terminal excerpt selected for this update."
	input := terminalshot.Input{
		ANSI: message, Title: firstNonEmpty(session.Title, capture.Title), Target: fmt.Sprintf("[%d]", session.ID),
		CWD: capture.CurrentPath, Columns: max(capture.Columns, 48), VisibleRows: capture.VisibleRows,
		BufferRows: 1, Compact: true, Footer: "no verified terminal evidence",
	}
	return guidedEvidenceCrop{input: input, plain: message, source: guidedEvidenceNone, hash: guidedCropHash(input, theme, guidedEvidenceNone)}
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
		Footer:        "verified terminal evidence",
	}
	return guidedEvidenceCrop{input: input, plain: strings.Join(plainRows[start:end+1], "\n"), source: guidedEvidenceVerified, hash: guidedCropHash(input, theme, guidedEvidenceVerified)}, true
}

func buildGuidedRecentActivityCrop(session state.TerminalSession, capture tmux.StyledCapture, previous conversationFrame, theme string) (guidedEvidenceCrop, bool) {
	if previous.physicalText == "" || previous.serverID == "" || previous.serverID != capture.ServerID ||
		previous.windowID == "" || previous.windowID != capture.WindowID || previous.paneID == "" || previous.paneID != capture.PaneID ||
		previous.command == "" || previous.command != strings.TrimSpace(capture.CurrentCmd) || previous.alternateOn != capture.AlternateOn ||
		previous.paneInMode != capture.PaneInMode || previous.columns != capture.Columns || previous.visibleRows != capture.VisibleRows {
		return guidedEvidenceCrop{}, false
	}
	oldRows := captureRows(previous.physicalText)
	newRows := captureRows(capture.Text)
	ansiRows := captureRows(capture.ANSI)
	if len(oldRows) == 0 || len(newRows) == 0 || len(newRows) != len(ansiRows) {
		return guidedEvidenceCrop{}, false
	}
	oldMatched, newMatched := lcsLineMatches(oldRows, newRows)
	if !strongConversationAlignment(oldRows, newRows, oldMatched, newMatched) {
		return guidedEvidenceCrop{}, false
	}
	first, last := -1, -1
	for index := 0; index < len(newRows); {
		if newMatched[index] {
			index++
			continue
		}
		start := index
		meaningful := false
		for index < len(newRows) && !newMatched[index] {
			meaningful = meaningful || strings.TrimSpace(newRows[index]) != ""
			index++
		}
		if meaningful {
			first, last = start, index-1
		}
	}
	if first < 0 {
		return guidedEvidenceCrop{}, false
	}
	start := max(0, first-guidedEvidenceContextRows)
	end := min(len(newRows)-1, last+guidedEvidenceContextRows)
	if end-start+1 > guidedEvidenceMaxRows {
		start = max(0, end-guidedEvidenceMaxRows+1)
	}
	highlights := make([]int, 0, end-start+1)
	for row := max(first, start); row <= min(last, end); row++ {
		if !newMatched[row] && strings.TrimSpace(newRows[row]) != "" {
			highlights = append(highlights, row-start)
		}
	}
	if len(highlights) == 0 {
		return guidedEvidenceCrop{}, false
	}
	return buildGuidedRangeCrop(session, capture, newRows, ansiRows, start, end, highlights, "recent terminal activity", guidedEvidenceRecent, theme), true
}

func buildGuidedTailCrop(session state.TerminalSession, capture tmux.StyledCapture, theme string) (guidedEvidenceCrop, bool) {
	plainRows := captureRows(capture.Text)
	ansiRows := captureRows(capture.ANSI)
	if len(plainRows) == 0 || len(plainRows) != len(ansiRows) {
		return guidedEvidenceCrop{}, false
	}
	end := len(plainRows) - 1
	for end >= 0 && strings.TrimSpace(plainRows[end]) == "" {
		end--
	}
	if end < 0 {
		return guidedEvidenceCrop{}, false
	}
	start := end
	for start > 0 && end-start+1 < guidedTailRows && strings.TrimSpace(plainRows[start-1]) != "" {
		start--
	}
	return buildGuidedRangeCrop(session, capture, plainRows, ansiRows, start, end, nil, "current terminal tail", guidedEvidenceTail, theme), true
}

func buildGuidedRangeCrop(session state.TerminalSession, capture tmux.StyledCapture, plainRows, ansiRows []string, start, end int, highlights []int, footer, source, theme string) guidedEvidenceCrop {
	input := terminalshot.Input{
		ANSI: strings.Join(ansiRows[start:end+1], "\n"), Title: firstNonEmpty(session.Title, capture.Title), Target: fmt.Sprintf("[%d]", session.ID),
		CWD: capture.CurrentPath, Columns: capture.Columns, VisibleRows: capture.VisibleRows, BufferRows: end - start + 1,
		Compact: true, HighlightRows: highlights, Footer: footer,
	}
	return guidedEvidenceCrop{input: input, plain: strings.Join(plainRows[start:end+1], "\n"), source: source, hash: guidedCropHash(input, theme, source)}
}

func guidedCropHash(input terminalshot.Input, theme, source string) string {
	return sha(strings.Join([]string{input.ANSI, fmt.Sprint(input.HighlightRows), input.Title, input.CWD, input.Footer, theme, source}, "\x00"))
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
