package app

import (
	"context"
	"strings"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const (
	maxConversationSummaryBytes = 8 * 1024
	maxConversationDeltaBytes   = 8 * 1024
	maxStableContextLines       = 8
)

type conversationGate struct {
	token chan struct{}
	refs  int
}

type conversationFrame struct {
	serverID     string
	windowID     string
	paneID       string
	command      string
	alternateOn  string
	paneInMode   string
	columns      int
	visibleRows  int
	text         string
	physicalText string
}

type conversationEpoch struct {
	frame         conversationFrame
	summary       string
	resetRevision uint64
}

type conversationTurn struct {
	input         guide.Input
	frame         conversationFrame
	previousFrame conversationFrame
	resetRevision uint64
}

func (a *App) acquireConversation(ctx context.Context, id int) (func(), bool) {
	a.conversationGateMu.Lock()
	if a.conversationGates == nil {
		a.conversationGates = make(map[int]*conversationGate)
	}
	gate := a.conversationGates[id]
	if gate == nil {
		gate = &conversationGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		a.conversationGates[id] = gate
	}
	gate.refs++
	a.conversationGateMu.Unlock()

	select {
	case <-gate.token:
		return func() {
			gate.token <- struct{}{}
			a.releaseConversationGate(id, gate)
		}, true
	case <-ctx.Done():
		a.releaseConversationGate(id, gate)
		return nil, false
	}
}

func (a *App) releaseConversationGate(id int, gate *conversationGate) {
	a.conversationGateMu.Lock()
	defer a.conversationGateMu.Unlock()
	gate.refs--
	if gate.refs == 0 && a.conversationGates[id] == gate {
		delete(a.conversationGates, id)
	}
}

func (a *App) prepareConversationTurn(session state.TerminalSession, capture tmux.StyledCapture, text string) conversationTurn {
	if a.Store != nil {
		a.pruneConversationEpochs(a.Store.Snapshot().TerminalSessions)
	}
	frame := conversationFrame{
		serverID:     capture.ServerID,
		windowID:     capture.WindowID,
		paneID:       capture.PaneID,
		command:      strings.TrimSpace(capture.CurrentCmd),
		alternateOn:  capture.AlternateOn,
		paneInMode:   capture.PaneInMode,
		columns:      capture.Columns,
		visibleRows:  capture.VisibleRows,
		text:         text,
		physicalText: capture.Text,
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	epoch := a.conversationEpochs[session.ID]
	if epoch.resetRevision == 0 {
		epoch.resetRevision = a.nextConversationRevisionLocked()
		a.conversationEpochs[session.ID] = epoch
	}
	turn := conversationTurn{
		frame:         frame,
		resetRevision: epoch.resetRevision,
		input: guide.Input{
			SessionID:   session.ID,
			VisibleText: text,
			Compact:     session.Collapsed,
		},
	}
	changed, removed, stable, ok := alignedConversationDelta(epoch.frame, frame)
	if ok {
		turn.previousFrame = epoch.frame
	}
	if ok && epoch.summary != "" && len(changed)+len(removed)+len(stable) <= maxConversationDeltaBytes {
		turn.input.PreviousRendering = tailUTF8(epoch.summary, maxConversationSummaryBytes)
		turn.input.ChangedText = changed
		turn.input.RemovedText = removed
		turn.input.StableContext = stable
	} else if epoch.frame.text != "" {
		epoch = conversationEpoch{resetRevision: a.nextConversationRevisionLocked()}
		a.conversationEpochs[session.ID] = epoch
		turn.resetRevision = epoch.resetRevision
	}
	return turn
}

func (a *App) conversationTurnCurrent(session state.TerminalSession, turn conversationTurn) bool {
	if a.Store == nil {
		return false
	}
	latest, ok := a.Store.FindSession(session.ID)
	if !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || latest.TmuxServerID != session.TmuxServerID ||
		latest.TmuxWindowID != session.TmuxWindowID || latest.TmuxPaneID != session.TmuxPaneID || latest.Collapsed != session.Collapsed {
		return false
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	return a.conversationEpochs[session.ID].resetRevision == turn.resetRevision
}

func (a *App) commitConversationTurn(session state.TerminalSession, turn conversationTurn, summary string) bool {
	if !a.conversationTurnCurrent(session, turn) {
		return false
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	epoch := a.conversationEpochs[session.ID]
	if epoch.resetRevision != turn.resetRevision {
		return false
	}
	epoch.frame = turn.frame
	epoch.summary = tailUTF8(summary, maxConversationSummaryBytes)
	a.conversationEpochs[session.ID] = epoch
	return true
}

func (a *App) resetConversationEpoch(sessionID int) {
	lock := a.anchorMutex(sessionID)
	lock.Lock()
	defer lock.Unlock()
	a.resetConversationEpochLocked(sessionID)
}

func (a *App) resetConversationEpochLocked(sessionID int) {
	a.conversationMu.Lock()
	a.ensureConversationEpochsLocked()
	a.conversationEpochs[sessionID] = conversationEpoch{resetRevision: a.nextConversationRevisionLocked()}
	a.conversationMu.Unlock()
	a.clearAgentFrame(sessionID)
}

func (a *App) pruneConversationEpochs(sessions []state.TerminalSession) {
	active := make(map[int]bool, len(sessions))
	for _, session := range sessions {
		active[session.ID] = true
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	for id := range a.conversationEpochs {
		if !active[id] {
			delete(a.conversationEpochs, id)
		}
	}
}

func (a *App) ensureConversationEpochsLocked() {
	if a.conversationEpochs == nil {
		a.conversationEpochs = make(map[int]conversationEpoch)
	}
}

func (a *App) nextConversationRevisionLocked() uint64 {
	a.conversationRevision++
	if a.conversationRevision == 0 {
		a.conversationRevision++
	}
	return a.conversationRevision
}

func alignedConversationDelta(previous, current conversationFrame) (changed, removed, stable string, ok bool) {
	if previous.text == "" || current.text == "" || previous.paneID == "" || current.paneID == "" ||
		previous.serverID == "" || previous.serverID != current.serverID || previous.windowID == "" || previous.windowID != current.windowID ||
		previous.paneID != current.paneID || previous.command == "" || previous.command != current.command ||
		previous.alternateOn != current.alternateOn || previous.paneInMode != current.paneInMode ||
		previous.columns != current.columns || previous.visibleRows != current.visibleRows {
		return "", "", "", false
	}
	oldLines := conversationLines(previous.text)
	newLines := conversationLines(current.text)
	if len(oldLines) == 0 || len(newLines) == 0 {
		return "", "", "", false
	}
	oldMatched, newMatched := lcsLineMatches(oldLines, newLines)
	if !strongConversationAlignment(oldLines, newLines, oldMatched, newMatched) {
		return "", "", "", false
	}
	changedLines := make([]string, 0)
	removedLines := make([]string, 0)
	contextIndexes := make(map[int]bool)
	for index, line := range newLines {
		if newMatched[index] {
			continue
		}
		changedLines = append(changedLines, line)
		for neighbor := index - 1; neighbor <= index+1; neighbor++ {
			if neighbor >= 0 && neighbor < len(newLines) && newMatched[neighbor] {
				contextIndexes[neighbor] = true
			}
		}
	}
	for index, line := range oldLines {
		if !oldMatched[index] {
			removedLines = append(removedLines, line)
		}
	}
	if len(changedLines) == 0 && len(removedLines) == 0 {
		return "", "", "", false
	}
	stableLines := make([]string, 0, maxStableContextLines)
	for index, line := range newLines {
		if contextIndexes[index] && len(stableLines) < maxStableContextLines {
			stableLines = append(stableLines, line)
		}
	}
	return strings.Join(changedLines, "\n"), strings.Join(removedLines, "\n"), strings.Join(stableLines, "\n"), true
}

func strongConversationAlignment(oldLines, newLines []string, oldMatched, newMatched []bool) bool {
	oldCounts := lineCounts(oldLines)
	newCounts := lineCounts(newLines)
	oldInformative, newInformative, common := 0, 0, 0
	for index, line := range oldLines {
		if informativeConversationLine(line, oldCounts) {
			oldInformative++
			if oldMatched[index] && informativeConversationLine(line, newCounts) {
				common++
			}
		}
	}
	for _, line := range newLines {
		if informativeConversationLine(line, newCounts) {
			newInformative++
		}
	}
	maximum := oldInformative
	if newInformative > maximum {
		maximum = newInformative
	}
	return common >= 3 && maximum > 0 && common*100 >= maximum*60
}

func informativeConversationLine(line string, counts map[string]int) bool {
	line = strings.TrimSpace(line)
	return len(line) >= 3 && counts[line] == 1
}

func lineCounts(lines []string) map[string]int {
	counts := make(map[string]int, len(lines))
	for _, line := range lines {
		counts[strings.TrimSpace(line)]++
	}
	return counts
}

func conversationLines(text string) []string {
	text = strings.Trim(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func lcsLineMatches(oldLines, newLines []string) (oldMatched, newMatched []bool) {
	table := make([][]int, len(oldLines)+1)
	for i := range table {
		table[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	oldMatched = make([]bool, len(oldLines))
	newMatched = make([]bool, len(newLines))
	for i, j := 0, 0; i < len(oldLines) && j < len(newLines); {
		if oldLines[i] == newLines[j] {
			oldMatched[i], newMatched[j] = true, true
			i++
			j++
		} else if table[i+1][j] >= table[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return oldMatched, newMatched
}
