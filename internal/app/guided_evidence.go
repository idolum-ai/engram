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

type guidedEvidenceCrop struct {
	input terminalshot.Input
	plain string
	hash  string
}

func (a *App) updateGuidedAnchorWithEvidence(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture, summary string, refs visibleReferences, excerpts []string, force bool, guard, accepted func() bool) bool {
	if !a.snapshotReady || a.Snapshots == nil || a.snapshotAnchors() {
		return false
	}
	safeExcerpts := make([]string, 0, len(excerpts))
	for _, excerpt := range excerpts {
		if a.redactText(excerpt) == excerpt {
			safeExcerpts = append(safeExcerpts, excerpt)
		}
	}
	crop, verified := buildGuidedEvidenceCrop(expected, capture, safeExcerpts, a.Config.SnapshotTheme)
	if !verified || a.redactText(crop.plain) != crop.plain {
		crop = buildGuidedEvidencePlaceholder(expected, capture, a.Config.SnapshotTheme)
	}
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
		_ = a.audit("terminal.guided_evidence", "replaced", map[string]any{"session_id": latest.ID, "verified": verified, "rows": crop.input.BufferRows})
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
	_ = a.audit("terminal.guided_evidence", "updated", map[string]any{"session_id": latest.ID, "verified": verified, "rows": crop.input.BufferRows})
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

func buildGuidedEvidencePlaceholder(session state.TerminalSession, capture tmux.StyledCapture, theme string) guidedEvidenceCrop {
	const message = "No verified terminal excerpt selected for this update."
	input := terminalshot.Input{
		ANSI: message, Title: firstNonEmpty(session.Title, capture.Title), Target: fmt.Sprintf("[%d]", session.ID),
		CWD: capture.CurrentPath, Columns: max(capture.Columns, 48), VisibleRows: capture.VisibleRows,
		BufferRows: 1, Compact: true, Footer: "no verified terminal evidence",
	}
	return guidedEvidenceCrop{input: input, plain: message, hash: sha(strings.Join([]string{message, input.Title, input.CWD, theme}, "\x00"))}
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
