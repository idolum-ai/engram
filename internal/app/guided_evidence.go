package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const guidedEvidenceMaxRows = 18
const guidedEvidenceContextRows = 2
const guidedEvidenceMinExcerptBytes = 12
const guidedCaptionBytes = 960
const guidedTailRows = 10

const (
	guidedEvidenceExcerpt = "model_excerpt"
	guidedEvidenceChanged = "changed_region"
	guidedEvidenceRelated = "summary_related"
	guidedEvidenceTail    = "terminal_tail"
	guidedEvidencePlain   = "plain_tail"
	guidedEvidenceGuide   = "guide_only"
)

type guidedEvidenceCrop struct {
	input  terminalshot.Input
	plain  string
	hash   string
	source string
}

func (a *App) updateGuidedAnchorReferences(ctx context.Context, expected state.TerminalSession, refs visibleReferences) bool {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	disclosureLock := a.disclosureMutex(expected.ID)
	disclosureLock.Lock()
	defer disclosureLock.Unlock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	current, ok := a.Store.FindSession(expected.ID)
	if !ok || current.State != state.TerminalRunning || !current.WatchEnabled || current.AnchorFormat != anchorFormatGuideEvidence || current.RetiringAnchorMessageID != 0 || !sameTerminalBinding(current, expected) {
		return false
	}
	frame, ok := a.snapshotTextFrame(current)
	if !ok {
		return false
	}
	caption, files := a.guidedEvidenceCaption(current, current.LastSummary, refs)
	presentationHash := firstNonEmpty(frame.PresentationHash, frame.FrameHash)
	renderHash := sha(caption + "\x00" + presentationHash)
	if renderHash == current.LastRenderHash {
		return true
	}
	presented := bindAnchorFiles(current, files)
	if _, err := a.Telegram.EditHTMLCaption(ctx, current.AnchorChatID, current.AnchorMessageID, telegram.MarkdownToHTML(caption), a.anchorMarkup(presented)); err != nil && !telegram.IsMessageNotModified(err) {
		_ = a.audit("telegram.guided_references", "failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		return false
	}
	updated := false
	if _, _, err := a.Store.UpdateSession(current.ID, func(session *state.TerminalSession) {
		if session.State == state.TerminalRunning && session.WatchEnabled && session.AnchorMessageID == current.AnchorMessageID && session.AnchorFormat == anchorFormatGuideEvidence && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, current) {
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			setAnchorFiles(session, files)
			updated = true
		}
	}); err != nil {
		_ = a.audit("state.guided_references", "failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		return false
	}
	return updated
}

func (a *App) updateGuidedAnchorWithEvidence(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture, previous conversationFrame, semanticText, summary string, refs visibleReferences, excerpts []string, force bool, guard, accepted func() bool) bool {
	if !a.snapshotReady || a.Snapshots == nil || a.snapshotAnchors() {
		return false
	}
	crop := a.selectGuidedEvidenceCrop(expected, capture, previous, semanticText, summary, excerpts)
	if !acquireSlot(ctx, a.renderSlots) {
		return false
	}
	renderCtx, cancel := context.WithTimeout(ctx, snapshotRenderTimeout)
	input := a.withSnapshotFooterStatus(renderCtx, crop.input, capture.CurrentPath)
	pngPath, renderErr := a.Snapshots.Render(renderCtx, input, a.Config.ArtifactDir())
	cancel()
	releaseSlot(a.renderSlots)
	if renderErr != nil {
		_ = a.audit("terminal.guided_evidence", "render_failed", map[string]any{"session_id": expected.ID, "error": renderErr.Error()})
		return false
	}
	defer os.Remove(pngPath)

	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	disclosureLock := a.disclosureMutex(expected.ID)
	disclosureLock.Lock()
	defer disclosureLock.Unlock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, expected.ID)
	latest, ok := a.Store.FindSession(expected.ID)
	if a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || !sameTerminalBinding(latest, expected) || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || guard != nil && !guard() {
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
		// The image already displayed by Telegram is this exact crop. Rebuild its
		// complete process-local View companion after restart before taking the
		// quiet path.
		a.rememberGuidedViewTextFrame(latest, capture, crop.hash)
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
			if !a.snapshotAnchors() && session.State == state.TerminalRunning && session.WatchEnabled && session.AnchorMessageID == oldID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, latest) {
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
		if current, found := a.Store.FindSession(latest.ID); found {
			// Telegram already exposes Raw on the replacement card. Keep its complete
			// View companion available even if later continuity acceptance loses a race.
			a.rememberGuidedViewTextFrame(current, capture, crop.hash)
		}
		return finish()
	}
	// Telegram now displays this crop under the existing message identity. Keep
	// its complete View companion coherent even if later state acceptance fails.
	a.rememberGuidedViewTextFrame(latest, capture, crop.hash)
	if guard != nil && !guard() {
		return false
	}
	updated := false
	_, _, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if !a.snapshotAnchors() && session.State == state.TerminalRunning && session.WatchEnabled && session.AnchorMessageID == latest.AnchorMessageID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, latest) {
			session.AnchorFormat = anchorFormatGuideEvidence
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			setAnchorFiles(session, files)
			updated = true
		}
	})
	committed := updated && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if stateErr != nil {
		outcome := "failed"
		if committed {
			outcome = "durability_uncertain"
		}
		_ = a.audit("state.guided_evidence", outcome, map[string]any{"session_id": latest.ID, "error": stateErr.Error()})
	}
	if !committed {
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
	if presentation := terminalPresentationText(session); presentation != "" {
		header += "\n" + a.redactText(presentation)
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

func (a *App) selectGuidedEvidenceCrop(session state.TerminalSession, capture tmux.StyledCapture, previous conversationFrame, semanticText, summary string, excerpts []string) guidedEvidenceCrop {
	session.Title = a.redactText(session.Title)
	capture.Title = a.redactText(capture.Title)
	capture.CurrentPath = a.redactText(capture.CurrentPath)
	safeExcerpts := make([]string, 0, len(excerpts))
	semanticRows := captureRows(semanticText)
	for _, excerpt := range excerpts {
		if a.redactText(excerpt) == excerpt {
			if _, ok := matchEvidenceRows(semanticRows, excerpt); !ok {
				continue
			}
			safeExcerpts = append(safeExcerpts, excerpt)
		}
	}
	builders := []func() (guidedEvidenceCrop, bool){
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedEvidenceCrop(session, capture, safeExcerpts, a.Config.SnapshotTheme)
		},
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedRecentActivityCrop(session, trimPassiveCapture(capture), previous, a.Config.SnapshotTheme)
		},
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedSummaryRelatedCrop(session, capture, semanticText, summary, a.Config.SnapshotTheme)
		},
		func() (guidedEvidenceCrop, bool) {
			return buildGuidedTailCrop(session, trimPassiveCapture(capture), a.Config.SnapshotTheme)
		},
	}
	for _, build := range builders {
		if crop, ok := build(); ok && a.redactText(crop.plain) == crop.plain {
			return crop
		}
	}
	trimmedCapture := trimPassiveCapture(capture)
	if crop, ok := buildGuidedPlainTailCrop(session, trimmedCapture, a.redactText(trimmedCapture.Text), a.Config.SnapshotTheme); ok {
		return crop
	}
	return buildGuidedOnlyFrame(session, capture, a.Config.SnapshotTheme)
}

type guidedBlockCandidate struct {
	text  string
	score int
	start int
}

func buildGuidedSummaryRelatedCrop(session state.TerminalSession, capture tmux.StyledCapture, semanticText, summary, theme string) (guidedEvidenceCrop, bool) {
	summaryTerms := guidedTermSet(summary)
	semanticRows := captureRows(semanticText)
	candidates := make([]guidedBlockCandidate, 0, 8)
	for start := 0; start < len(semanticRows); {
		for start < len(semanticRows) && strings.TrimSpace(semanticRows[start]) == "" {
			start++
		}
		if start == len(semanticRows) {
			break
		}
		end := start
		for end+1 < len(semanticRows) && strings.TrimSpace(semanticRows[end+1]) != "" {
			end++
		}
		rows := semanticRows[start : end+1]
		text := strings.Join(rows, "\n")
		if len(rows) <= guidedEvidenceMaxRows && !passiveGuidedBlock(rows) {
			overlap := guidedTermOverlap(summaryTerms, guidedTermSet(text))
			hasReference := strings.Contains(text, "https://") || strings.Contains(text, "http://")
			if overlap >= 2 || hasReference {
				score := overlap*10 + min(len([]rune(strings.Join(strings.Fields(text), " ")))/80, 5)
				if hasReference {
					score += 25
				}
				candidates = append(candidates, guidedBlockCandidate{text: text, score: score, start: start})
			}
		}
		start = end + 1
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].start > candidates[j].start
	})
	for _, candidate := range candidates {
		crop, ok := buildGuidedEvidenceCrop(session, capture, []string{candidate.text}, theme)
		if !ok {
			continue
		}
		crop.input.Footer = "related terminal text"
		crop.source = guidedEvidenceRelated
		crop.hash = guidedCropHash(crop.input, theme, crop.source)
		return crop, true
	}
	return guidedEvidenceCrop{}, false
}

func passiveGuidedBlock(rows []string) bool {
	if len(rows) == 0 {
		return true
	}
	first := strings.ToLower(strings.TrimSpace(strings.TrimLeft(rows[0], "•*-✓✔ ")))
	if strings.HasPrefix(first, "updated plan") {
		return true
	}
	for _, row := range rows {
		trimmed := strings.TrimSpace(row)
		if isPassiveTerminalFooter(trimmed) {
			return true
		}
		if strings.HasPrefix(trimmed, "›") && isPassivePromptSuggestion(strings.ToLower(strings.Join(strings.Fields(strings.TrimPrefix(trimmed, "›")), " "))) {
			return true
		}
	}
	return false
}

func guidedTermSet(text string) map[string]bool {
	stop := map[string]bool{
		"and": true, "are": true, "but": true, "for": true, "from": true, "has": true,
		"have": true, "into": true, "its": true, "not": true, "the": true, "then": true,
		"this": true, "was": true, "were": true, "with": true, "you": true,
	}
	terms := make(map[string]bool)
	for _, term := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len([]rune(term)) >= 3 && !stop[term] {
			terms[term] = true
		}
	}
	return terms
}

func guidedTermOverlap(left, right map[string]bool) int {
	overlap := 0
	for term := range left {
		if right[term] {
			overlap++
		}
	}
	return overlap
}

func buildGuidedOnlyFrame(session state.TerminalSession, capture tmux.StyledCapture, theme string) guidedEvidenceCrop {
	input := terminalshot.Input{
		ANSI: " ", Title: firstNonEmpty(session.Title, capture.Title), Target: fmt.Sprintf("[%d]", session.ID),
		CWD: capture.CurrentPath, Columns: max(capture.Columns, 48), VisibleRows: capture.VisibleRows,
		BufferRows: 1, Compact: true, Footer: "guided view",
	}
	return guidedEvidenceCrop{input: input, source: guidedEvidenceGuide, hash: guidedCropHash(input, theme, guidedEvidenceGuide)}
}

func buildGuidedEvidenceCrop(session state.TerminalSession, capture tmux.StyledCapture, excerpts []string, theme string) (guidedEvidenceCrop, bool) {
	plainRows := captureRows(capture.Text)
	ansiRows := captureRows(capture.ANSI)
	if len(plainRows) == 0 || len(plainRows) != len(ansiRows) {
		return guidedEvidenceCrop{}, false
	}
	selected := make(map[int]bool)
	for _, excerpt := range excerpts {
		match, ok := matchEvidenceSpan(plainRows, excerpt)
		if !ok {
			continue
		}
		for row := match.rows[0]; row <= match.rows[1]; row++ {
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
	start, end := boundedEvidenceBlock(plainRows, first, last, guidedEvidenceMaxRows)
	highlights := make([]int, 0, len(indices))
	for _, row := range indices {
		highlights = append(highlights, row-start)
	}
	return buildGuidedRangeCrop(session, capture, plainRows, ansiRows, start, end, highlights, "quoted terminal text", guidedEvidenceExcerpt, theme), true
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
	if last-first+1 > guidedEvidenceMaxRows {
		first = last - guidedEvidenceMaxRows + 1
	}
	start, end := boundedEvidenceBlock(newRows, first, last, guidedEvidenceMaxRows)
	highlights := make([]int, 0, end-start+1)
	for row := max(first, start); row <= min(last, end); row++ {
		if !newMatched[row] && strings.TrimSpace(newRows[row]) != "" {
			highlights = append(highlights, row-start)
		}
	}
	if len(highlights) == 0 {
		return guidedEvidenceCrop{}, false
	}
	return buildGuidedRangeCrop(session, capture, newRows, ansiRows, start, end, highlights, "changed terminal region", guidedEvidenceChanged, theme), true
}

func boundedEvidenceBlock(rows []string, first, last, maxRows int) (int, int) {
	first = max(0, first)
	last = min(len(rows)-1, last)
	if first > last || maxRows <= 0 {
		return first, last
	}
	start, end := first, last
	for context := 0; start > 0 && context < guidedEvidenceContextRows && strings.TrimSpace(rows[start-1]) != "" && end-start+1 < maxRows; context++ {
		start--
	}
	for context := 0; end+1 < len(rows) && context < guidedEvidenceContextRows && strings.TrimSpace(rows[end+1]) != "" && end-start+1 < maxRows; context++ {
		end++
	}
	return start, end
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

func buildGuidedPlainTailCrop(session state.TerminalSession, capture tmux.StyledCapture, text, theme string) (guidedEvidenceCrop, bool) {
	rows := captureRows(text)
	end := len(rows) - 1
	for end >= 0 && strings.TrimSpace(rows[end]) == "" {
		end--
	}
	if end < 0 {
		return guidedEvidenceCrop{}, false
	}
	start := end
	for start > 0 && end-start+1 < guidedTailRows && strings.TrimSpace(rows[start-1]) != "" {
		start--
	}
	return buildGuidedRangeCrop(session, capture, rows, rows, start, end, nil, "current terminal tail", guidedEvidencePlain, theme), true
}

func buildGuidedRangeCrop(session state.TerminalSession, capture tmux.StyledCapture, plainRows, ansiRows []string, start, end int, highlights []int, footer, source, theme string) guidedEvidenceCrop {
	cropANSI := append([]string(nil), ansiRows[start:end+1]...)
	if prefix := inheritedSGRPrefix(ansiRows[:start]); prefix != "" && len(cropANSI) > 0 {
		cropANSI[0] = prefix + cropANSI[0]
	}
	cropPlain := append([]string(nil), plainRows[start:end+1]...)
	input := terminalshot.Input{
		ANSI: strings.Join(cropANSI, "\n"), Title: firstNonEmpty(session.Title, capture.Title), Target: fmt.Sprintf("[%d]", session.ID),
		CWD: capture.CurrentPath, Columns: capture.Columns, VisibleRows: capture.VisibleRows, BufferRows: end - start + 1,
		Compact: true, HighlightRows: highlights, Footer: footer,
	}
	return guidedEvidenceCrop{input: input, plain: strings.Join(cropPlain, "\n"), source: source, hash: guidedCropHash(input, theme, source)}
}

func guidedCropHash(input terminalshot.Input, theme, source string) string {
	return sha(strings.Join([]string{input.ANSI, fmt.Sprint(input.HighlightRows), fmt.Sprint(input.Columns), fmt.Sprint(input.VisibleRows), fmt.Sprint(input.BufferRows), input.Title, input.CWD, input.Footer, theme, source}, "\x00"))
}

func trimPassiveCapture(capture tmux.StyledCapture) tmux.StyledCapture {
	filtered := conversationEvidence(capture.Text)
	if filtered == capture.Text {
		return capture
	}
	plainRows := captureRows(filtered)
	physicalRows := captureRows(capture.Text)
	ansiRows := captureRows(capture.ANSI)
	if len(physicalRows) != len(ansiRows) || len(plainRows) > len(ansiRows) {
		return capture
	}
	capture.Text = filtered
	capture.ANSI = strings.Join(ansiRows[:len(plainRows)], "\n")
	capture.BufferRows = len(plainRows)
	return capture
}

func inheritedSGRPrefix(rows []string) string {
	var out strings.Builder
	for _, row := range rows {
		for index := 0; index+2 < len(row); {
			start := strings.Index(row[index:], "\x1b[")
			if start < 0 {
				break
			}
			start += index
			end := start + 2
			for end < len(row) && (row[end] < 0x40 || row[end] > 0x7e) {
				end++
			}
			if end >= len(row) {
				break
			}
			if row[end] == 'm' {
				out.WriteString(row[start : end+1])
			}
			index = end + 1
		}
	}
	return out.String()
}

type terminalPosition struct {
	row    int
	column int
	width  int
}

type evidenceMatch struct {
	rows    [2]int
	columns [2]int
}

func matchEvidenceRows(rows []string, excerpt string) ([2]int, bool) {
	match, ok := matchEvidenceSpan(rows, excerpt)
	return match.rows, ok
}

func matchEvidenceSpan(rows []string, excerpt string) (evidenceMatch, bool) {
	needle := strings.Join(strings.Fields(excerpt), " ")
	if len(needle) < guidedEvidenceMinExcerptBytes || len(strings.Fields(needle)) < 2 {
		return evidenceMatch{}, false
	}
	haystack, positions := normalizeTerminalRows(rows)
	needleRunes := []rune(needle)
	matchStart := runeSliceIndex(haystack, needleRunes, 0)
	if matchStart < 0 || runeSliceIndex(haystack, needleRunes, matchStart+1) >= 0 {
		return evidenceMatch{}, false
	}
	matchEnd := matchStart + len(needleRunes)
	firstRow, lastRow := positions[matchStart].row, positions[matchEnd-1].row
	left, right := positions[matchStart].column, positions[matchStart].column
	for _, position := range positions[matchStart:matchEnd] {
		left = min(left, position.column)
		right = max(right, position.column+max(position.width, 1)-1)
	}
	return evidenceMatch{rows: [2]int{firstRow, lastRow}, columns: [2]int{left, right}}, true
}

func normalizeTerminalRows(rows []string) ([]rune, []terminalPosition) {
	var normalized []rune
	var positions []terminalPosition
	pendingSpace := false
	for rowIndex, row := range rows {
		column := 0
		for _, r := range row {
			width := terminalRuneWidth(r, column)
			if unicode.IsSpace(r) {
				if len(normalized) > 0 {
					pendingSpace = true
				}
				column += width
				continue
			}
			if pendingSpace {
				normalized = append(normalized, ' ')
				positions = append(positions, terminalPosition{row: rowIndex, column: column, width: 1})
				pendingSpace = false
			}
			normalized = append(normalized, r)
			positions = append(positions, terminalPosition{row: rowIndex, column: column, width: width})
			column += width
		}
		if len(normalized) > 0 {
			pendingSpace = true
		}
	}
	return normalized, positions
}

func runeSliceIndex(haystack, needle []rune, start int) int {
	for index := max(0, start); index+len(needle) <= len(haystack); index++ {
		match := true
		for offset := range needle {
			if haystack[index+offset] != needle[offset] {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}

func terminalRuneWidth(r rune, column int) int {
	if r == '\t' {
		return 8 - column%8
	}
	if r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if r < 0x20 || r == 0x7f {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f ||
		r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff ||
		r >= 0xfe10 && r <= 0xfe19 || r >= 0xfe30 && r <= 0xfe6f ||
		r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 ||
		r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

func captureRows(text string) []string {
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
}
