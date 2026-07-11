package app

import "github.com/idolum-ai/engram/internal/state"

const maxStaleAlternateMessages = 16

func recordAlternateMessage(session *state.TerminalSession, kind string, messageID int) {
	if messageID == 0 {
		return
	}
	var previous int
	switch kind {
	case "summary":
		previous = session.SummaryMessageID
		session.SummaryMessageID = messageID
	case "snapshot":
		previous = session.SnapshotMessageID
		session.SnapshotMessageID = messageID
	default:
		return
	}
	stale := session.StaleAlternateMessageIDs[:0]
	for _, id := range session.StaleAlternateMessageIDs {
		if id != messageID {
			stale = append(stale, id)
		}
	}
	session.StaleAlternateMessageIDs = stale
	recordStaleMessage(session, previous)
}

func recordStaleMessage(session *state.TerminalSession, messageID int) {
	if messageID == 0 || messageID == session.AnchorMessageID || messageID == session.SummaryMessageID || messageID == session.SnapshotMessageID {
		return
	}
	stale := session.StaleAlternateMessageIDs[:0]
	for _, id := range session.StaleAlternateMessageIDs {
		if id != messageID {
			stale = append(stale, id)
		}
	}
	stale = append(stale, messageID)
	if len(stale) > maxStaleAlternateMessages {
		stale = stale[len(stale)-maxStaleAlternateMessages:]
	}
	session.StaleAlternateMessageIDs = stale
}
