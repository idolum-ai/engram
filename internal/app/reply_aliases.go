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
	case "evidence":
		previous = session.EvidenceMessageID
		session.EvidenceMessageID = messageID
	case "upstream":
		previous = session.UpstreamMessageID
		session.UpstreamMessageID = messageID
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
	if messageID == 0 || messageID == session.AnchorMessageID || messageID == session.SummaryMessageID || messageID == session.SnapshotMessageID || messageID == session.EvidenceMessageID || messageID == session.UpstreamMessageID {
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

func retireAlternateReplyTargets(session *state.TerminalSession) {
	messageIDs := []int{session.SummaryMessageID, session.SnapshotMessageID, session.EvidenceMessageID, session.UpstreamMessageID}
	session.SummaryMessageID = 0
	session.SnapshotMessageID = 0
	session.EvidenceMessageID = 0
	session.EvidenceAnchorMessageID = 0
	session.LastEvidenceHash = ""
	session.UpstreamMessageID = 0
	for _, messageID := range messageIDs {
		recordStaleMessage(session, messageID)
	}
}
