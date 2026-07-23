package agentui

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxFrameRows = 64

var (
	elapsedDecoration = regexp.MustCompile(`(?i)^[─━═]+\s*(?:worked|ran|completed)\s+for\s+(?:[0-9]+h\s+)?(?:[0-9]+m\s+)?[0-9]+s\s*[─━═]+$`)
	glyphElapsed      = regexp.MustCompile(`(?i)^[^\pL\pN\s]\s*[\pL]+\s+for\s+(?:[0-9]+h\s+)?(?:[0-9]+m\s+)?[0-9]+s$`)
	durationStatus    = regexp.MustCompile(`\((?:[0-9]+h\s+)?(?:[0-9]+m\s+)?[0-9]+s(?:\s*[•·;][^)]*)?\)`)
	displayModel      = regexp.MustCompile(`(?i)\b(sonnet|opus|haiku|gemini|gpt)[ -]([0-9]+(?:\.[0-9]+)*)(?:[ -]([a-z][a-z0-9]*))?\b`)
)

// Analyze conservatively interprets one bounded terminal frame. A recognized
// model status line acts as the structural anchor. Without that anchor, or
// when the frame exceeds the bound, Analyze returns the text byte-for-byte.
func Analyze(observation Observation) Analysis {
	original := observation.Current.Text
	analysis := Analysis{Original: original, Conversation: original, Activity: ActivityUnknown}
	if strings.TrimSpace(original) == "" {
		return analysis
	}
	lines := strings.Split(original, "\n")
	if len(lines) > maxFrameRows {
		return analysis
	}

	footer, model, effort, mode, ok := findStatusFooter(lines)
	modelLine := -1
	if !ok {
		footer, model, effort, mode, ok = findEmbeddedStatus(lines)
	}
	if !ok {
		footer, model, effort, modelLine, ok = findCompositeStatus(lines)
	}
	if !ok || !sufficientStructuralAnchor(observation, lines, footer) {
		return analysis
	}

	remove := make([]bool, len(lines))
	roles := make([]Role, len(lines))
	confidence := make([]int, len(lines))
	evidence := make([][]string, len(lines))
	markLine(remove, roles, confidence, evidence, footer, RoleStatus, 100, true, "known-model", "low-band-status")

	activity := ActivityIdle
	for index := max(0, footer-8); index < min(len(lines), footer+9); index++ {
		if index == footer {
			continue
		}
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" {
			continue
		}
		switch {
		case approvalActivity(trimmed):
			activity = ActivityAwaitingApproval
			markLine(remove, roles, confidence, evidence, index, RoleActivity, 98, true, "approval-language", "low-band-status")
		case explicitActivity(trimmed):
			activity = ActivityActive
			markLine(remove, roles, confidence, evidence, index, RoleActivity, 98, true, "duration-status", "interrupt-hint")
		case spinnerActivity(trimmed):
			activity = ActivityActive
			markLine(remove, roles, confidence, evidence, index, RoleActivity, 92, true, "spinner-glyph", "low-band-status")
		case temporalActivity(observation, lines, index):
			activity = ActivityActive
			markLine(remove, roles, confidence, evidence, index, RoleActivity, 90, true, "changed-between-frames", "low-band-status")
		}
	}

	annotateConversation(lines, roles, confidence, evidence)
	markChrome(lines, footer, model, remove, roles, confidence, evidence)
	markTrailingPrompt(observation, lines, footer, remove, roles, confidence, evidence)
	markModelCard(lines, modelLine, remove, roles, confidence, evidence)
	markCompletedApproval(lines, footer, remove, roles, confidence, evidence)
	if hasRole(roles, RoleApproval) {
		activity = ActivityAwaitingApproval
	}

	kept := make([]string, 0, len(lines))
	for index, line := range lines {
		if !remove[index] {
			kept = append(kept, line)
		}
	}
	cleaned := strings.Trim(strings.Join(kept, "\n"), "\n")
	if strings.TrimSpace(cleaned) == "" {
		// Chrome by itself still contains useful evidence. Fail closed instead of
		// replacing a complete frame with an empty presentation.
		return analysis
	}

	analysis.Conversation = cleaned
	analysis.Model = model
	analysis.Effort = effort
	analysis.Mode = mode
	analysis.Activity = activity
	analysis.Confidence = 100
	analysis.Applied = true
	analysis.Regions = regionsFor(lines, roles, confidence, evidence, remove)
	return analysis
}

func sufficientStructuralAnchor(observation Observation, lines []string, footer int) bool {
	if strings.Count(lines[footer], " · ") >= 2 {
		return true
	}
	for index := max(0, footer-10); index < min(len(lines), footer+10); index++ {
		if index == footer {
			continue
		}
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, "❯") ||
			explicitActivity(trimmed) || separatorLine(trimmed) || elapsedDecoration.MatchString(trimmed) ||
			decorativeBorder(trimmed) || uiCommandHints(trimmed) || spinnerActivity(trimmed) ||
			collapsedTranscript(trimmed) || strings.HasPrefix(trimmed, "└") {
			return true
		}
	}
	if observation.Previous == nil || !compatibleFrames(observation.Current, *observation.Previous) {
		return false
	}
	footerText := strings.TrimSpace(lines[footer])
	previousLines := strings.Split(observation.Previous.Text, "\n")
	for index := len(previousLines) - 1; index >= 0 && index >= len(previousLines)-10; index-- {
		if strings.TrimSpace(previousLines[index]) == footerText {
			return true
		}
	}
	return false
}

func findStatusFooter(lines []string) (index int, model, effort, mode string, ok bool) {
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for index = last; index >= 0 && index >= last-9; index-- {
		trimmed := strings.TrimSpace(lines[index])
		parts := strings.Split(trimmed, " · ")
		if len(parts) < 2 || !validStatusTail(parts[1:]) {
			continue
		}
		identity := strings.Fields(parts[0])
		if len(identity) == 0 || !knownModel(identity[0]) {
			continue
		}

		candidateEffort := ""
		candidateMode := ""
		valid := true
		for _, field := range identity[1:] {
			switch {
			case validEffort(field) && candidateEffort == "":
				candidateEffort = strings.ToLower(field)
			case strings.EqualFold(field, "fast") && candidateMode == "":
				candidateMode = "fast"
			default:
				valid = false
			}
		}
		if !valid {
			continue
		}
		return index, identity[0], candidateEffort, candidateMode, true
	}
	return 0, "", "", "", false
}

func findEmbeddedStatus(lines []string) (index int, model, effort, mode string, ok bool) {
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for index = last; index >= 0 && index >= last-9; index-- {
		parts := strings.Split(strings.TrimSpace(lines[index]), " · ")
		if len(parts) != 2 {
			continue
		}
		identity := strings.Fields(parts[0])
		if len(identity) != 2 || identity[0] != "▣" || !validEmbeddedLabel(identity[1]) {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) == 0 || !knownModel(fields[0]) {
			continue
		}
		candidateModel := fields[0]
		candidateEffort := ""
		candidateMode := ""
		valid := true
		for _, field := range fields[1:] {
			switch {
			case candidateEffort == "" && validEffort(field):
				candidateEffort = strings.ToLower(field)
			case candidateMode == "" && strings.EqualFold(field, "fast"):
				candidateMode = "fast"
			default:
				valid = false
			}
		}
		if valid {
			return index, candidateModel, candidateEffort, candidateMode, true
		}
	}
	return 0, "", "", "", false
}

func validEmbeddedLabel(value string) bool {
	if len(value) < 2 || len(value) > 24 {
		return false
	}
	for _, r := range value {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func findCompositeStatus(lines []string) (index int, model, effort string, modelLine int, ok bool) {
	model, modelLine = findDisplayModel(lines)
	if model == "" {
		return 0, "", "", -1, false
	}
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for index = last; index >= 0 && index >= last-9; index-- {
		parts := strings.Split(strings.TrimSpace(lines[index]), " · ")
		if len(parts) < 2 {
			continue
		}
		for _, part := range parts {
			for _, field := range strings.Fields(part) {
				field = strings.Trim(field, "●○◉◌")
				if validEffort(field) {
					return index, model, strings.ToLower(field), modelLine, true
				}
			}
		}
	}
	return 0, "", "", -1, false
}

func findDisplayModel(lines []string) (string, int) {
	for index := len(lines) - 1; index >= 0; index-- {
		match := displayModel.FindStringSubmatch(lines[index])
		if len(match) == 0 || !boxedModelLine(lines, index) {
			continue
		}
		family := strings.ToLower(match[1])
		version := strings.ReplaceAll(match[2], ".", "-")
		suffix := strings.ToLower(match[3])
		switch family {
		case "sonnet", "opus", "haiku":
			return "claude-" + family + "-" + version, index
		case "gemini":
			model := "gemini-" + version
			if suffix != "" {
				model += "-" + suffix
			}
			return model, index
		case "gpt":
			model := "gpt-" + match[2]
			if suffix != "" {
				model += "-" + suffix
			}
			if knownModel(model) {
				return model, index
			}
		}
	}
	return "", -1
}

func boxedModelLine(lines []string, modelLine int) bool {
	start := -1
	for index := modelLine; index >= max(0, modelLine-12); index-- {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "╭") && strings.HasSuffix(trimmed, "╮") {
			start = index
			break
		}
	}
	if start < 0 {
		return false
	}
	for index := modelLine; index < len(lines) && index <= modelLine+12; index++ {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "╰") && strings.HasSuffix(trimmed, "╯") {
			return true
		}
	}
	return false
}

func validStatusTail(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	first := strings.TrimSpace(parts[0])
	if first == "" {
		return false
	}
	if strings.HasPrefix(first, "/") || strings.HasPrefix(first, "~/") || first == "~" {
		return true
	}
	// Some agent TUIs put branch or context metadata before the path. Requiring
	// a second separated field keeps a prose sentence from becoming a footer.
	return len(parts) >= 2
}

func knownModel(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 96 {
		return false
	}
	if strings.Contains(value, "/") {
		parts := strings.Split(value, "/")
		if len(parts) != 2 || !knownProvider(parts[0]) || !validModelToken(parts[0]) {
			return false
		}
		value = parts[1]
	}
	if strings.HasPrefix(value, "gpt-") {
		return validGPTModel(value)
	}
	if strings.HasPrefix(value, "claude-") {
		return validNamedModel(value, "claude-")
	}
	if strings.HasPrefix(value, "gemini-") {
		return validNamedModel(value, "gemini-")
	}
	for _, prefix := range []string{"deepseek-", "grok-", "kimi-", "llama-", "mistral-", "qwen-"} {
		if strings.HasPrefix(value, prefix) {
			return validNamedModel(value, prefix)
		}
	}
	for _, family := range []string{"o1", "o3", "o4"} {
		if value == family || strings.HasPrefix(value, family+"-") {
			return validModelToken(value)
		}
	}
	return false
}

func knownProvider(value string) bool {
	switch value {
	case "openai", "anthropic", "google", "xai", "deepseek", "meta", "mistral", "moonshot", "qwen", "local", "engram":
		return true
	default:
		return false
	}
}

func validGPTModel(value string) bool {
	suffix := strings.TrimPrefix(value, "gpt-")
	if suffix == "" {
		return false
	}
	index := consumeDigits(suffix, 0)
	if index == 0 {
		return false
	}
	for index < len(suffix) && suffix[index] == '.' {
		index++
		next := consumeDigits(suffix, index)
		if next == index {
			return false
		}
		index = next
	}
	if index < len(suffix) && suffix[index] == 'o' {
		index++
	}
	for index < len(suffix) {
		if suffix[index] != '-' {
			return false
		}
		index++
		start := index
		for index < len(suffix) && (isASCIILower(suffix[index]) || isASCIIDigit(suffix[index])) {
			index++
		}
		if start == index {
			return false
		}
	}
	return validModelToken(value)
}

func validNamedModel(value, prefix string) bool {
	return len(value) > len(prefix) && validModelToken(value)
}

func validModelToken(value string) bool {
	if len(value) < 2 || len(value) > 96 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_", r) {
			continue
		}
		return false
	}
	return true
}

func consumeDigits(value string, start int) int {
	for start < len(value) && isASCIIDigit(value[start]) {
		start++
	}
	return start
}

func isASCIIDigit(value byte) bool { return value >= '0' && value <= '9' }
func isASCIILower(value byte) bool { return value >= 'a' && value <= 'z' }

func validEffort(value string) bool {
	switch strings.ToLower(value) {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func explicitActivity(line string) bool {
	lower := strings.ToLower(line)
	return durationStatus.MatchString(line) && (strings.Contains(lower, "interrupt") || strings.ContainsAny(line, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"))
}

func spinnerActivity(line string) bool {
	if glyphElapsed.MatchString(line) {
		return false
	}
	lower := strings.ToLower(line)
	return strings.HasPrefix(line, "✻") || strings.HasPrefix(line, "✽") || strings.HasPrefix(line, "✶") ||
		strings.Contains(lower, "interrupt") && strings.ContainsAny(line, "⬝⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
}

func approvalActivity(line string) bool {
	lower := strings.ToLower(line)
	return explicitActivity(line) && strings.Contains(lower, "approval")
}

func temporalActivity(observation Observation, current []string, index int) bool {
	previous := observation.Previous
	if previous == nil || index < 0 || index >= len(current) || !compatibleFrames(observation.Current, *previous) {
		return false
	}
	line := strings.TrimSpace(current[index])
	if line == "" || !durationStatus.MatchString(line) {
		return false
	}
	prior := strings.Split(previous.Text, "\n")
	priorIndex := len(prior) - (len(current) - index)
	return priorIndex >= 0 && priorIndex < len(prior) && strings.TrimSpace(prior[priorIndex]) != line
}

func compatibleFrames(current, previous Frame) bool {
	return sameWhenKnown(current.CurrentCommand, previous.CurrentCommand) &&
		sameIntWhenKnown(current.Columns, previous.Columns) &&
		sameIntWhenKnown(current.VisibleRows, previous.VisibleRows) &&
		sameWhenKnown(current.AlternateScreen, previous.AlternateScreen) &&
		sameWhenKnown(current.CopyMode, previous.CopyMode)
}

func sameWhenKnown(left, right string) bool { return left == "" || right == "" || left == right }
func sameIntWhenKnown(left, right int) bool { return left == 0 || right == 0 || left == right }

func markTrailingPrompt(observation Observation, lines []string, footer int, remove []bool, roles []Role, confidence []int, evidence [][]string) {
	end := footer - 1
	for end >= 0 && (strings.TrimSpace(lines[end]) == "" || remove[end]) {
		end--
	}
	if end < max(0, footer-5) {
		return
	}
	start := end
	for start > max(0, footer-5) && strings.TrimSpace(lines[start-1]) != "" && !remove[start-1] {
		start--
	}
	first := strings.TrimSpace(lines[start])
	if !strings.HasPrefix(first, "›") && !strings.HasPrefix(first, "❯") {
		return
	}

	promptLines := append([]string(nil), lines[start:end+1]...)
	promptLines[0] = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(promptLines[0]), "›❯"))
	prompt := strings.ToLower(strings.Join(strings.Fields(strings.Join(promptLines, " ")), " "))
	passive := prompt == "" || passivePrompt(prompt) || activityBeforePrompt(roles, start, footer) ||
		promptPersisted(observation.Previous, lines, start, end)
	role := RoleUserMessage
	score := 92
	signals := []string{"prompt-glyph", "adjacent-to-status"}
	if passive {
		role = RoleComposer
		score = 98
		signals = append(signals, "passive-placeholder")
	}
	for index := start; index <= end; index++ {
		markLine(remove, roles, confidence, evidence, index, role, score, passive, signals...)
	}
}

func activityBeforePrompt(roles []Role, start, footer int) bool {
	for index := max(0, footer-8); index < start; index++ {
		if roles[index] == RoleActivity {
			return true
		}
	}
	return false
}

func promptPersisted(previous *Frame, current []string, start, end int) bool {
	if previous == nil {
		return false
	}
	prior := strings.Split(previous.Text, "\n")
	offset := len(prior) - len(current)
	priorStart := start + offset
	priorEnd := end + offset
	if priorStart < 0 || priorEnd >= len(prior) {
		return false
	}
	return slices.Equal(prior[priorStart:priorEnd+1], current[start:end+1])
}

func passivePrompt(prompt string) bool {
	switch prompt {
	case "write tests for @filename", "run /review on my current changes", "find and fix a bug in @filename", "summarize recent commits", "implement {feature}", "explain this codebase":
		return true
	default:
		return false
	}
}

func markChrome(lines []string, footer int, model string, remove []bool, roles []Role, confidence []int, evidence [][]string) {
	for index := range lines {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		inLowBand := index >= max(0, footer-8) && index < min(len(lines), footer+9)
		switch {
		case roles[index] != "":
			continue
		case separatorLine(trimmed):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 100, true, "separator-decoration")
		case inLowBand && decorativeBorder(trimmed):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 98, true, "composer-border")
		case inLowBand && uiCommandHints(trimmed):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 94, true, "command-hints")
		case inLowBand && (elapsedDecoration.MatchString(trimmed) || glyphElapsed.MatchString(trimmed)):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 98, true, "elapsed-decoration")
		case inLowBand && collapsedTranscript(trimmed):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 98, true, "collapsed-transcript-control")
		case inLowBand && embeddedComposerStatus(lines, index, model):
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 96, true, "embedded-composer-status", "low-band-status")
		}
	}
}

func embeddedComposerStatus(lines []string, index int, model string) bool {
	trimmed := strings.TrimSpace(lines[index])
	if !strings.HasPrefix(trimmed, "┃") || !strings.Contains(trimmed, " · "+model) || index+1 >= len(lines) {
		return false
	}
	return decorativeBorder(strings.TrimSpace(lines[index+1]))
}

func markModelCard(lines []string, modelLine int, remove []bool, roles []Role, confidence []int, evidence [][]string) {
	if modelLine < 0 || modelLine >= len(lines) {
		return
	}
	start := -1
	for index := modelLine; index >= max(0, modelLine-12); index-- {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "╭") && strings.HasSuffix(trimmed, "╮") {
			start = index
			break
		}
	}
	if start < 0 {
		return
	}
	end := -1
	for index := modelLine; index < len(lines) && index <= modelLine+12; index++ {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "╰") && strings.HasSuffix(trimmed, "╯") {
			end = index
			break
		}
	}
	if end < start {
		return
	}
	for index := start; index <= end; index++ {
		markLine(remove, roles, confidence, evidence, index, RoleChrome, 96, true, "boxed-model-card")
	}
}

func collapsedTranscript(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(line, "… +") && strings.Contains(lower, "lines") && strings.Contains(lower, "transcript")
}

func markCompletedApproval(lines []string, footer int, remove []bool, roles []Role, confidence []int, evidence [][]string) {
	for index := max(0, footer-8); index < min(len(lines), footer+1); index++ {
		if roles[index] != "" {
			continue
		}
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "⚠ Automatic approval review approved:") {
			markLine(remove, roles, confidence, evidence, index, RoleChrome, 95, true, "completed-approval-boilerplate", "low-band-status")
		}
	}
}

func separatorLine(line string) bool {
	if utf8.RuneCountInString(line) < 8 {
		return false
	}
	for _, r := range line {
		if r != '─' && r != '━' && r != '═' {
			return false
		}
	}
	return true
}

func decorativeBorder(line string) bool {
	if utf8.RuneCountInString(line) < 8 {
		return false
	}
	for _, r := range line {
		if !strings.ContainsRune("─━═▀▄╹╺╸┄┅┈┉ ", r) {
			return false
		}
	}
	return true
}

func uiCommandHints(line string) bool {
	lower := strings.ToLower(line)
	count := 0
	for _, hint := range []string{"ctrl+", "tab ", "shift+", "esc ", "/effort", " agents"} {
		if strings.Contains(lower, hint) {
			count++
		}
	}
	return count >= 2
}

func annotateConversation(lines []string, roles []Role, confidence []int, evidence [][]string) {
	for index, line := range lines {
		if roles[index] != "" || strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, "❯"):
			markRole(roles, confidence, evidence, index, RoleUserMessage, 88, "prompt-glyph")
		case strings.HasPrefix(trimmed, "• Ran ") || strings.HasPrefix(trimmed, "• Running "):
			markRole(roles, confidence, evidence, index, RoleToolInvocation, 92, "execution-verb")
		case strings.HasPrefix(trimmed, "└"):
			markRole(roles, confidence, evidence, index, RoleToolResult, 90, "result-tree-glyph")
		case approvalPrompt(trimmed):
			markRole(roles, confidence, evidence, index, RoleApproval, 88, "approval-question")
		case strings.HasPrefix(trimmed, "•") || strings.HasPrefix(trimmed, "⏺"):
			markRole(roles, confidence, evidence, index, RoleAssistant, 72, "agent-bullet")
		}
	}
}

func approvalPrompt(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "do you want to") || strings.Contains(lower, "would you like to") || strings.Contains(lower, "allow this")
}

func hasRole(roles []Role, target Role) bool {
	return slices.Contains(roles, target)
}

func markLine(remove []bool, roles []Role, confidence []int, evidence [][]string, index int, role Role, score int, omitted bool, signals ...string) {
	if index < 0 || index >= len(roles) || score < confidence[index] {
		return
	}
	markRole(roles, confidence, evidence, index, role, score, signals...)
	if omitted {
		remove[index] = true
	}
}

func markRole(roles []Role, confidence []int, evidence [][]string, index int, role Role, score int, signals ...string) {
	roles[index] = role
	confidence[index] = score
	evidence[index] = append([]string(nil), signals...)
}

func regionsFor(lines []string, roles []Role, confidence []int, evidence [][]string, remove []bool) []Region {
	regions := make([]Region, 0)
	for index := 0; index < len(lines); {
		if roles[index] == "" {
			index++
			continue
		}
		region := Region{
			StartLine: index, EndLine: index, Role: roles[index], Confidence: confidence[index],
			Evidence: append([]string(nil), evidence[index]...), Omitted: remove[index],
		}
		for region.EndLine+1 < len(lines) && sameRegion(region, roles, confidence, evidence, remove, region.EndLine+1) {
			region.EndLine++
		}
		regions = append(regions, region)
		index = region.EndLine + 1
	}
	return regions
}

func sameRegion(region Region, roles []Role, confidence []int, evidence [][]string, remove []bool, index int) bool {
	return roles[index] == region.Role && confidence[index] == region.Confidence &&
		remove[index] == region.Omitted && slices.Equal(evidence[index], region.Evidence)
}
