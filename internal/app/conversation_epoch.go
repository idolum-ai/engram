package app

import (
	"strings"
	"sync"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const (
	maxConversationSummaryBytes = 8 * 1024
	maxConversationInputBytes   = 2 * 1024
	maxConversationDeltaBytes   = 16 * 1024
	maxStableContextLines       = 8
)

type conversationFrame struct {
	serverID    string
	windowID    string
	paneID      string
	command     string
	columns     int
	visibleRows int
	text        string
}

type conversationEpoch struct {
	frame         conversationFrame
	summary       string
	pendingInput  string
	inputRevision uint64
	resetRevision uint64
}

type conversationTurn struct {
	input         anthropic.ConversationInput
	frame         conversationFrame
	inputRevision uint64
	resetRevision uint64
}

func (a *App) conversationMutex(id int) *sync.Mutex {
	lock, _ := a.conversationLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (a *App) prepareConversationTurn(session state.TerminalSession, capture tmux.StyledCapture, text string) conversationTurn {
	frame := conversationFrame{
		serverID:    session.TmuxServerID,
		windowID:    session.TmuxWindowID,
		paneID:      session.TmuxPaneID,
		command:     strings.TrimSpace(capture.CurrentCmd),
		columns:     capture.Columns,
		visibleRows: capture.VisibleRows,
		text:        text,
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	epoch := a.conversationEpochs[session.ID]
	turn := conversationTurn{
		frame:         frame,
		inputRevision: epoch.inputRevision,
		resetRevision: epoch.resetRevision,
		input: anthropic.ConversationInput{
			SessionID:   session.ID,
			VisibleText: text,
		},
	}
	if conversationInputVisible(epoch.pendingInput, epoch.frame.text, frame.text) {
		turn.input.RecentUserInput = tailUTF8(epoch.pendingInput, maxConversationInputBytes)
	}
	if changed, stable, ok := alignedConversationDelta(epoch.frame, frame); ok && epoch.summary != "" {
		turn.input.VisibleText = ""
		turn.input.PreviousRendering = tailUTF8(epoch.summary, maxConversationSummaryBytes)
		turn.input.ChangedText = tailUTF8(changed, maxConversationDeltaBytes)
		turn.input.StableContext = tailUTF8(stable, maxConversationDeltaBytes/4)
	}
	return turn
}

func conversationInputVisible(input string, frames ...string) bool {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > maxConversationInputBytes {
		return false
	}
	for _, frame := range frames {
		if strings.Contains(frame, input) {
			return true
		}
	}
	return false
}

func (a *App) commitConversationTurn(sessionID int, turn conversationTurn, summary string) {
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	epoch := a.conversationEpochs[sessionID]
	if epoch.resetRevision != turn.resetRevision {
		return
	}
	epoch.frame = turn.frame
	epoch.summary = tailUTF8(summary, maxConversationSummaryBytes)
	if epoch.inputRevision == turn.inputRevision {
		epoch.pendingInput = ""
	}
	a.conversationEpochs[sessionID] = epoch
}

func (a *App) noteConversationInput(sessionID int, input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	if len(input) > maxConversationInputBytes {
		input = ""
	}
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	epoch := a.conversationEpochs[sessionID]
	epoch.pendingInput = input
	epoch.inputRevision++
	a.conversationEpochs[sessionID] = epoch
}

func (a *App) resetConversationEpoch(sessionID int) {
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	a.ensureConversationEpochsLocked()
	revision := a.conversationEpochs[sessionID].resetRevision + 1
	a.conversationEpochs[sessionID] = conversationEpoch{resetRevision: revision}
}

func (a *App) ensureConversationEpochsLocked() {
	if a.conversationEpochs == nil {
		a.conversationEpochs = make(map[int]conversationEpoch)
	}
}

func alignedConversationDelta(previous, current conversationFrame) (changed, stable string, ok bool) {
	if previous.text == "" || current.text == "" || previous.paneID == "" || current.paneID == "" ||
		previous.serverID == "" || previous.serverID != current.serverID || previous.windowID == "" || previous.windowID != current.windowID ||
		previous.paneID != current.paneID || previous.command == "" || previous.command != current.command ||
		previous.columns != current.columns || previous.visibleRows != current.visibleRows {
		return "", "", false
	}
	oldLines := conversationLines(previous.text)
	newLines := conversationLines(current.text)
	if len(oldLines) == 0 || len(newLines) == 0 {
		return "", "", false
	}
	matched := lcsCurrentLines(oldLines, newLines)
	common := 0
	for _, isMatched := range matched {
		if isMatched {
			common++
		}
	}
	minimum := len(oldLines)
	if len(newLines) < minimum {
		minimum = len(newLines)
	}
	required := (minimum + 3) / 4
	if required < 1 {
		required = 1
	}
	if common < required {
		return "", "", false
	}
	changedLines := make([]string, 0, len(newLines)-common)
	contextIndexes := make(map[int]bool)
	for index, line := range newLines {
		if matched[index] {
			continue
		}
		changedLines = append(changedLines, line)
		for neighbor := index - 1; neighbor <= index+1; neighbor++ {
			if neighbor >= 0 && neighbor < len(newLines) && matched[neighbor] {
				contextIndexes[neighbor] = true
			}
		}
	}
	if len(changedLines) == 0 {
		return "", "", false
	}
	stableLines := make([]string, 0, maxStableContextLines)
	for index, line := range newLines {
		if contextIndexes[index] && len(stableLines) < maxStableContextLines {
			stableLines = append(stableLines, line)
		}
	}
	return strings.Join(changedLines, "\n"), strings.Join(stableLines, "\n"), true
}

func conversationLines(text string) []string {
	text = strings.Trim(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func lcsCurrentLines(oldLines, newLines []string) []bool {
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
	matched := make([]bool, len(newLines))
	for i, j := 0, 0; i < len(oldLines) && j < len(newLines); {
		if oldLines[i] == newLines[j] {
			matched[j] = true
			i++
			j++
		} else if table[i+1][j] >= table[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return matched
}
