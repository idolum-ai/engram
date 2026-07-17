package app

const (
	anchorFormatText          = "text"
	anchorFormatSnapshot      = "snapshot"
	anchorFormatGuideEvidence = "guide-evidence"
)

func mediaAnchorFormat(format string) bool {
	return format == anchorFormatSnapshot || format == anchorFormatGuideEvidence
}

func guideAnchorFormat(format string) bool {
	return firstNonEmpty(format, anchorFormatText) == anchorFormatText || format == anchorFormatGuideEvidence
}
