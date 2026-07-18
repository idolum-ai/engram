package terminalshot

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"image/png"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	LogicalWidth                  = 430
	LogicalHeight                 = 932
	PixelRatio                    = 3
	TargetRows                    = 64
	maxInputBytes                 = 1 << 20
	probeTimeout                  = 15 * time.Second
	terminalWidth                 = 406.0
	terminalCharRatio             = 0.602
	maxTerminalFont               = 9.4
	minTerminalFont               = 7.0
	terminalLineHeight            = 1.4
	wrappedColumns                = 100
	footerStatusMaxCells          = 24
	footerStatusMinCells          = 8
	footerStatusCellPixels        = 9 * terminalCharRatio
	footerHorizontalPadding       = 24
	footerMinimumProvenancePixels = 150
	footerDimensionsPixels        = 78
	footerGapPixels               = 12
	maxStatusBytes                = 4 << 10
)

type Input struct {
	ANSI          string
	Title         string
	Target        string
	CWD           string
	Columns       int
	VisibleRows   int
	BufferRows    int
	Compact       bool
	HighlightRows []int
	Footer        string
	Status        string
}

type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("snapshot browser: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

type Renderer struct {
	Browser string
	Theme   string
	Runner  CommandRunner
}

func New(browser, theme string) *Renderer {
	return &Renderer{Browser: strings.TrimSpace(browser), Theme: strings.TrimSpace(theme), Runner: ExecRunner{}}
}

func (r *Renderer) Available() (string, error) {
	if r == nil {
		return "", fmt.Errorf("snapshot renderer is not configured")
	}
	return browserPath(r.Browser)
}

func (r *Renderer) Probe(ctx context.Context) (string, error) {
	browser, err := r.Available()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "engram-browser-probe-*")
	if err != nil {
		return "", fmt.Errorf("create snapshot browser probe directory: %w", err)
	}
	defer os.RemoveAll(dir)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	path, err := r.Render(probeCtx, Input{
		ANSI:        "Engram snapshot probe\n",
		Title:       "Engram",
		Target:      "probe",
		CWD:         "/",
		Columns:     24,
		VisibleRows: 1,
		BufferRows:  1,
	}, dir)
	if err != nil {
		return "", fmt.Errorf("snapshot browser probe: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove snapshot browser probe: %w", err)
	}
	return browser, nil
}

func (r *Renderer) Render(ctx context.Context, input Input, dir string) (string, error) {
	if err := validateInput(input); err != nil {
		return "", err
	}
	browser, err := r.Available()
	if err != nil {
		return "", err
	}
	runner := r.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}

	htmlFile, err := os.CreateTemp(dir, ".engram-snapshot-*.html")
	if err != nil {
		return "", fmt.Errorf("create snapshot HTML: %w", err)
	}
	htmlPath := htmlFile.Name()
	defer os.Remove(htmlPath)
	if err := htmlFile.Chmod(0o600); err != nil {
		htmlFile.Close()
		return "", fmt.Errorf("protect snapshot HTML: %w", err)
	}
	if _, err := io.WriteString(htmlFile, RenderHTML(input, r.Theme)); err != nil {
		htmlFile.Close()
		return "", fmt.Errorf("write snapshot HTML: %w", err)
	}
	if err := htmlFile.Close(); err != nil {
		return "", fmt.Errorf("close snapshot HTML: %w", err)
	}

	profileDir, err := os.MkdirTemp(dir, ".engram-browser-*")
	if err != nil {
		return "", fmt.Errorf("create browser profile: %w", err)
	}
	defer os.RemoveAll(profileDir)

	pngFile, err := os.CreateTemp(dir, "engram-window-*.png")
	if err != nil {
		return "", fmt.Errorf("reserve snapshot PNG: %w", err)
	}
	pngPath := pngFile.Name()
	if err := pngFile.Close(); err != nil {
		os.Remove(pngPath)
		return "", fmt.Errorf("close snapshot PNG reservation: %w", err)
	}
	if err := os.Remove(pngPath); err != nil {
		return "", fmt.Errorf("prepare snapshot PNG path: %w", err)
	}
	keepPNG := false
	defer func() {
		if !keepPNG {
			_ = os.Remove(pngPath)
		}
	}()

	pageURL := (&url.URL{Scheme: "file", Path: htmlPath}).String()
	logicalWidth := renderWidth(input)
	logicalHeight := renderHeight(input)
	args := []string{
		"--headless",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-gpu",
		"--disable-sync",
		"--hide-scrollbars",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-default-browser-check",
		"--no-first-run",
		"--force-device-scale-factor=" + strconv.Itoa(PixelRatio),
		fmt.Sprintf("--window-size=%d,%d", logicalWidth, logicalHeight),
		"--user-data-dir=" + profileDir,
		"--screenshot=" + pngPath,
		pageURL,
	}
	if err := runner.Run(ctx, browser, args...); err != nil {
		return "", err
	}
	info, err := os.Stat(pngPath)
	if err != nil {
		return "", fmt.Errorf("snapshot browser produced no PNG: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("snapshot browser produced an invalid PNG")
	}
	pngFile, err = os.Open(pngPath)
	if err != nil {
		return "", fmt.Errorf("open snapshot PNG: %w", err)
	}
	pngConfig, decodeErr := png.DecodeConfig(pngFile)
	closeErr := pngFile.Close()
	if decodeErr != nil {
		return "", fmt.Errorf("decode snapshot PNG: %w", decodeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close snapshot PNG: %w", closeErr)
	}
	if pngConfig.Width != logicalWidth*PixelRatio || pngConfig.Height != logicalHeight*PixelRatio {
		return "", fmt.Errorf("snapshot browser produced %dx%d PNG, want %dx%d", pngConfig.Width, pngConfig.Height, logicalWidth*PixelRatio, logicalHeight*PixelRatio)
	}
	if err := os.Chmod(pngPath, 0o600); err != nil {
		return "", fmt.Errorf("protect snapshot PNG: %w", err)
	}
	keepPNG = true
	return pngPath, nil
}

func RenderHTML(input Input, themeName string) string {
	bufferRows := input.BufferRows
	if bufferRows <= 0 {
		bufferRows = input.VisibleRows
	}
	layoutColumns := input.Columns
	if input.Compact {
		layoutColumns = min(layoutColumns, 71)
	}
	renderColumns, fontSize, lineHeight := readableTerminalLayout(layoutColumns)
	if input.Compact {
		lineHeight = 13.2
	}
	lineHeightCSS := fmt.Sprintf("%.2f", lineHeight)
	if input.Compact {
		lineHeightCSS = "13.2"
	}
	columnLabel := fmt.Sprintf("%dx%d visible", input.Columns, input.VisibleRows)
	if renderColumns < input.Columns {
		columnLabel = fmt.Sprintf("full %d columns · wrapped at %d · %d visible rows", input.Columns, renderColumns, input.VisibleRows)
	}
	theme := snapshotThemeFor(themeName)
	if input.Compact {
		theme.accessible = true
	}
	footer := fmt.Sprintf("%d-row bounded frame · %s", bufferRows, columnLabel)
	if input.Compact {
		footer = firstNonEmpty(input.Footer, "quoted terminal text")
	}
	whiteSpace, overflowWrap, wordBreak := "pre", "normal", "normal"
	visualRows := bufferRows
	wrapRows := input.Compact || renderColumns < input.Columns
	if wrapRows {
		whiteSpace, overflowWrap, wordBreak = "pre-wrap", "anywhere", "break-all"
		visualRows = snapshotVisualRows(input.ANSI, bufferRows, renderColumns)
	}
	highlights := renderHighlights(input.HighlightRows, theme, input.ANSI, bufferRows, renderColumns, lineHeight, wrapRows)
	// Desktop Chromium may clamp its CSS viewport above --window-size while
	// still clipping the PNG to the requested bitmap. Size the card itself from
	// Engram's canvas so its right and bottom chrome stay inside that bitmap.
	logicalWidth := renderWidth(input)
	logicalHeight := renderHeight(input)
	statusBudget := snapshotFooterStatusCellBudget(input)
	status := truncateTerminalCells(strings.TrimSpace(input.Status), statusBudget)
	statusHTML := ""
	if status != "" {
		statusHTML = fmt.Sprintf(`<span class="status">%s</span>`, html.EscapeString(status))
	}
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><style>
:root{color-scheme:%s}*{box-sizing:border-box}html,body{margin:0;overflow:hidden;background:%s}body{color:%s;font-synthesis:none}.window{width:%dpx;height:%dpx;overflow:hidden;background:%s}.bar{width:100%%;height:44px;display:flex;align-items:center;justify-content:space-between;gap:12px;padding:0 12px;overflow:hidden;border-bottom:1px solid %s;background:%s}.title{flex:0 1 58%%;min-width:0;overflow:hidden;color:%s;font:600 12px/1 system-ui,sans-serif;text-overflow:ellipsis;white-space:nowrap}.location{flex:1;min-width:0;overflow:hidden;color:%s;font:11px/1 system-ui,sans-serif;text-align:right;text-overflow:ellipsis;white-space:nowrap}.screen{position:relative;width:100%%;height:calc(100%% - 66px);padding:10px 12px 0;overflow:hidden;background:%s}.evidence-mark{position:absolute;left:8px;right:8px;z-index:0;border-left:3px solid %s;background:%s}pre{position:relative;z-index:1;width:%dch;height:%.2fpx;margin:0;overflow:hidden;color:%s;background:transparent;font:%.2fpx/%spx "JetBrains Mono","Cascadia Mono","SFMono-Regular",Menlo,Consolas,"DejaVu Sans Mono",monospace;font-variant-ligatures:none;letter-spacing:0;tab-size:8;white-space:%s;overflow-wrap:%s;word-break:%s}.foot{width:100%%;height:22px;display:flex;align-items:center;justify-content:space-between;gap:%dpx;padding:0 12px;overflow:hidden;border-top:1px solid %s;color:%s;background:%s;font:9px/1 system-ui,sans-serif}.foot span{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.provenance{flex:1 1 auto}.status{flex:0 1 %dch;max-width:%dch;text-align:right;font-family:"SFMono-Regular",Menlo,Consolas,"DejaVu Sans Mono",monospace}.dimensions{flex:0 0 auto;text-align:right}
</style></head><body><main class="window"><header class="bar"><div class="title">%s · tmux %s</div><div class="location">%s</div></header><section class="screen">%s<pre>%s</pre></section><footer class="foot"><span class="provenance">%s</span>%s<span class="dimensions">%dx%d visible</span></footer></main></body></html>`,
		theme.colorScheme, theme.canvas, theme.text, logicalWidth, logicalHeight, theme.screen, theme.border, theme.bar, theme.title, theme.muted, theme.screen,
		theme.highlightBorder, theme.highlight, renderColumns, float64(visualRows)*lineHeight, theme.text, fontSize, lineHeightCSS, whiteSpace, overflowWrap, wordBreak, footerGapPixels, theme.subtleBorder, theme.muted, theme.foot, statusBudget, statusBudget,
		html.EscapeString(firstNonEmpty(input.Title, "terminal")), html.EscapeString(input.Target), html.EscapeString(input.CWD), highlights, ansiHTML(input.ANSI, theme),
		html.EscapeString(footer), statusHTML, input.Columns, input.VisibleRows)
}

func snapshotFooterStatusCellBudget(input Input) int {
	availablePixels := renderWidth(input) - footerHorizontalPadding - footerMinimumProvenancePixels - footerDimensionsPixels - 2*footerGapPixels
	if availablePixels <= 0 {
		return 0
	}
	cells := int(math.Floor(float64(availablePixels) / footerStatusCellPixels))
	if cells < footerStatusMinCells {
		return 0
	}
	return min(cells, footerStatusMaxCells)
}

func truncateTerminalCells(value string, maxCells int) string {
	if maxCells <= 0 || value == "" {
		return ""
	}
	cells := 0
	for _, r := range value {
		cells += terminalRuneWidth(r, cells)
	}
	if cells <= maxCells {
		return value
	}
	if maxCells == 1 {
		return "…"
	}
	var out strings.Builder
	cells = 0
	for _, r := range value {
		width := terminalRuneWidth(r, cells)
		if cells+width > maxCells-1 {
			break
		}
		out.WriteRune(r)
		cells += width
	}
	return strings.TrimSpace(out.String()) + "…"
}

// RenderedColumns reports the readable wrap width. Full snapshots soft-wrap
// wider physical rows instead of clipping them.
func RenderedColumns(columns int) int {
	renderColumns, _, _ := readableTerminalLayout(columns)
	return renderColumns
}

func readableTerminalLayout(columns int) (renderColumns int, fontSize, lineHeight float64) {
	renderColumns = columns
	fontSize = maxTerminalFont
	if fit := terminalWidth / (float64(columns) * terminalCharRatio); fit < fontSize {
		fontSize = fit
	}
	if fontSize < minTerminalFont {
		fontSize = minTerminalFont
		renderColumns = min(columns, wrappedColumns)
	}
	return renderColumns, fontSize, fontSize * terminalLineHeight
}

func renderWidth(input Input) int {
	if input.Compact {
		return LogicalWidth
	}
	renderColumns, fontSize, _ := readableTerminalLayout(input.Columns)
	return max(LogicalWidth, int(math.Ceil(float64(renderColumns)*fontSize*terminalCharRatio))+24)
}

func renderHeight(input Input) int {
	if !input.Compact {
		renderColumns, _, lineHeight := readableTerminalLayout(input.Columns)
		if renderColumns >= input.Columns {
			return LogicalHeight
		}
		visualRows := snapshotVisualRows(input.ANSI, input.BufferRows, renderColumns)
		// The screen's border-box includes 10px of top padding. Reserve it in
		// addition to the 44px header and 22px footer so the final visual row
		// remains above the footer instead of being clipped by overflow:hidden.
		return max(LogicalHeight, 76+int(math.Ceil(float64(visualRows)*lineHeight)))
	}
	renderColumns := min(input.Columns, 71)
	visualRows := snapshotVisualRows(input.ANSI, input.BufferRows, renderColumns)
	height := 86 + int(math.Ceil(float64(visualRows)*13.2))
	if height < 180 {
		return 180
	}
	return height
}

func snapshotVisualRows(ansi string, bufferRows, columns int) int {
	counts := snapshotRowVisualCounts(ansi, bufferRows, columns)
	visualRows := 0
	for _, count := range counts {
		visualRows += count
	}
	return max(visualRows, bufferRows)
}

func snapshotRowVisualCounts(ansi string, bufferRows, columns int) []int {
	if bufferRows <= 0 || columns <= 0 {
		return []int{max(bufferRows, 1)}
	}
	plain := plainANSIText(ansi)
	rows := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	if plain == "" {
		rows = nil
	}
	counts := make([]int, max(bufferRows, len(rows)))
	for index := range counts {
		counts[index] = 1
	}
	for index, row := range rows {
		// tmux -N preserves padding; padding occupies the existing physical
		// row and must not manufacture extra soft-wrapped rows.
		row = strings.TrimRight(row, " \t")
		cells := 0
		for _, r := range row {
			cells += terminalRuneWidth(r, cells)
		}
		counts[index] = max(1, int(math.Ceil(float64(cells)/float64(columns))))
	}
	return counts
}

func plainANSIText(input string) string {
	var out strings.Builder
	for i := 0; i < len(input); {
		if input[i] == 0x1b {
			if i+1 < len(input) && input[i+1] == '[' {
				end := i + 2
				for end < len(input) && (input[end] < 0x40 || input[end] > 0x7e) {
					end++
				}
				if end < len(input) {
					i = end + 1
					continue
				}
			}
			if i+1 < len(input) && input[i+1] == ']' {
				_, next, ok := terminalStringEnd(input, i+2)
				if !ok {
					return out.String()
				}
				i = next
				continue
			}
			i++
			continue
		}
		end := i
		for end < len(input) && input[end] != 0x1b {
			end++
		}
		out.WriteString(cleanText(input[i:end]))
		i = end
	}
	return out.String()
}

// terminalStringEnd recognizes every string terminator tmux may preserve for
// OSC sequences: BEL, the 7-bit ST escape, raw C1 ST, and UTF-8 C1 ST.
func terminalStringEnd(input string, start int) (end, next int, ok bool) {
	for i := start; i < len(input); {
		switch {
		case input[i] == 0x07:
			return i, i + 1, true
		case input[i] == 0x1b && i+1 < len(input) && input[i+1] == '\\':
			return i, i + 2, true
		}
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == '\u009c' {
			return i, i + size, true
		}
		if r == utf8.RuneError && size == 1 {
			if input[i] == 0x9c {
				return i, i + 1, true
			}
			i++
			continue
		}
		i += size
	}
	return len(input), len(input), false
}

func renderHighlights(rows []int, theme snapshotTheme, ansi string, bufferRows, columns int, lineHeight float64, wrapped bool) string {
	selected := make(map[int]bool, len(rows))
	for _, row := range rows {
		selected[row] = true
	}
	counts := make([]int, max(bufferRows, 1))
	for index := range counts {
		counts[index] = 1
	}
	if wrapped {
		counts = snapshotRowVisualCounts(ansi, bufferRows, columns)
	}
	var out strings.Builder
	visualStart := 0
	for row, count := range counts {
		if selected[row] {
			fmt.Fprintf(&out, `<span class="evidence-mark" style="top:%.1fpx;height:%.1fpx"></span>`, 10+float64(visualStart)*lineHeight, float64(count)*lineHeight)
		}
		visualStart += count
	}
	return out.String()
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

func validateInput(input Input) error {
	if input.Columns <= 0 || input.Columns > 400 {
		return fmt.Errorf("snapshot columns must be between 1 and 400")
	}
	if input.VisibleRows <= 0 || input.VisibleRows > 400 {
		return fmt.Errorf("snapshot visible rows must be between 1 and 400")
	}
	if input.BufferRows <= 0 || input.BufferRows > 400 {
		return fmt.Errorf("snapshot buffer rows must be between 1 and 400")
	}
	if len(input.ANSI) > maxInputBytes {
		return fmt.Errorf("snapshot capture exceeds %d bytes", maxInputBytes)
	}
	if len(input.Status) > maxStatusBytes {
		return fmt.Errorf("snapshot status exceeds %d bytes", maxStatusBytes)
	}
	for _, row := range input.HighlightRows {
		if row < 0 || row >= input.BufferRows {
			return fmt.Errorf("snapshot highlight row %d is outside the capture", row)
		}
	}
	return nil
}

func browserPath(configured string) (string, error) {
	return browserPathForOS(configured, runtime.GOOS)
}

func browserPathForOS(configured, goos string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if strings.ContainsRune(configured, filepath.Separator) || filepath.IsAbs(configured) {
			info, err := os.Stat(configured)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return "", fmt.Errorf("configured snapshot browser is not executable: %s", configured)
			}
			return configured, nil
		}
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("configured snapshot browser not found: %s", configured)
		}
		return path, nil
	}
	candidates := []string{"chrome-headless-shell", "chromium-headless-shell", "headless_shell"}
	if goos != "darwin" {
		candidates = append(candidates, "chromium", "chromium-browser", "google-chrome", "google-chrome-stable")
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	if goos == "darwin" {
		return "", fmt.Errorf("no dedicated headless Chromium snapshot browser found; install chrome-headless-shell or set ENGRAM_SNAPSHOT_BROWSER explicitly")
	}
	return "", fmt.Errorf("no Chromium-compatible snapshot browser found")
}

type snapshotTheme struct {
	colorScheme                              string
	canvas, screen, bar, foot                string
	text, title, muted, border, subtleBorder string
	highlight, highlightBorder               string
	ansi                                     []string
	accessible                               bool
}

func snapshotThemeFor(name string) snapshotTheme {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "contrast-dark":
		return snapshotTheme{
			colorScheme: "dark", canvas: "#000000", screen: "#000000", bar: "#101010", foot: "#080808",
			text: "#ffffff", title: "#ffffff", muted: "#d8d8d8", border: "#ffffff", subtleBorder: "#a8a8a8",
			highlight: "rgba(255,230,109,.18)", highlightBorder: "#ffe66d",
			ansi:       []string{"#000000", "#ff8c42", "#63f2c8", "#ffe66d", "#8db7ff", "#ff9ee5", "#58d6ff", "#ffffff", "#b8b8b8", "#ffad7a", "#8affd8", "#fff29c", "#b8d1ff", "#ffc4ef", "#9ae8ff", "#ffffff"},
			accessible: true,
		}
	case "contrast-light":
		return snapshotTheme{
			colorScheme: "light", canvas: "#ffffff", screen: "#ffffff", bar: "#f2f2f2", foot: "#f7f7f7",
			text: "#111111", title: "#111111", muted: "#444444", border: "#111111", subtleBorder: "#666666",
			highlight: "rgba(23,78,166,.12)", highlightBorder: "#174ea6",
			ansi:       []string{"#111111", "#8f2f00", "#00623d", "#665200", "#174ea6", "#792168", "#00646d", "#333333", "#444444", "#a13a00", "#006b45", "#715d00", "#2458ad", "#862873", "#006f78", "#111111"},
			accessible: true,
		}
	default:
		return snapshotTheme{
			colorScheme: "dark", canvas: "#111418", screen: "#111418", bar: "#202429", foot: "#171a1e",
			text: "#d8dee9", title: "#c7ccd1", muted: "#858c94", border: "#30353a", subtleBorder: "#252a2f",
			highlight: "rgba(245,197,66,.16)", highlightBorder: "#f5c542",
			ansi: []string{"#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5", "#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff"},
		}
	}
}

type terminalStyle struct {
	fg, bg                    string
	bold, dim, italic         bool
	underline, strike, invert bool
}

func ansiHTML(input string, theme snapshotTheme) string {
	var out strings.Builder
	current := terminalStyle{}
	for i := 0; i < len(input); {
		if input[i] == 0x1b {
			if i+1 < len(input) && input[i+1] == '[' {
				end := i + 2
				for end < len(input) && (input[end] < 0x40 || input[end] > 0x7e) {
					end++
				}
				if end < len(input) {
					if input[end] == 'm' {
						applySGR(&current, input[i+2:end], theme)
					}
					i = end + 1
					continue
				}
			}
			if i+1 < len(input) && input[i+1] == ']' {
				_, next, ok := terminalStringEnd(input, i+2)
				if !ok {
					return out.String()
				}
				i = next
				continue
			}
			i++
			continue
		}
		end := i
		for end < len(input) && input[end] != 0x1b {
			end++
		}
		text := cleanText(input[i:end])
		if text != "" {
			out.WriteString("<span")
			if css := current.css(theme); css != "" {
				out.WriteString(` style="`)
				out.WriteString(css)
				out.WriteByte('"')
			}
			out.WriteByte('>')
			out.WriteString(html.EscapeString(text))
			out.WriteString("</span>")
		}
		i = end
	}
	return out.String()
}

func cleanText(text string) string {
	var out strings.Builder
	for _, r := range strings.ReplaceAll(text, "\r", "") {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func applySGR(s *terminalStyle, raw string, theme snapshotTheme) {
	raw = strings.ReplaceAll(raw, ":", ";")
	if raw == "" {
		raw = "0"
	}
	parts := strings.Split(raw, ";")
	values := make([]int, len(parts))
	for i, part := range parts {
		values[i], _ = strconv.Atoi(part)
	}
	for i := 0; i < len(values); i++ {
		n := values[i]
		switch {
		case n == 0:
			*s = terminalStyle{}
		case n == 1:
			s.bold = true
		case n == 2:
			s.dim = true
		case n == 3:
			s.italic = true
		case n == 4:
			s.underline = true
		case n == 7:
			s.invert = true
		case n == 9:
			s.strike = true
		case n == 22:
			s.bold, s.dim = false, false
		case n == 23:
			s.italic = false
		case n == 24:
			s.underline = false
		case n == 27:
			s.invert = false
		case n == 29:
			s.strike = false
		case n >= 30 && n <= 37:
			s.fg = xtermColor(n-30, theme)
		case n == 39:
			s.fg = ""
		case n >= 40 && n <= 47:
			s.bg = xtermColor(n-40, theme)
		case n == 49:
			s.bg = ""
		case n >= 90 && n <= 97:
			s.fg = xtermColor(n-90+8, theme)
		case n >= 100 && n <= 107:
			s.bg = xtermColor(n-100+8, theme)
		case (n == 38 || n == 48) && i+2 < len(values) && values[i+1] == 5:
			color := xtermColor(values[i+2], theme)
			if n == 38 {
				s.fg = color
			} else {
				s.bg = color
			}
			i += 2
		case (n == 38 || n == 48) && i+4 < len(values) && values[i+1] == 2:
			color := fmt.Sprintf("#%02x%02x%02x", clamp(values[i+2]), clamp(values[i+3]), clamp(values[i+4]))
			if n == 38 {
				s.fg = color
			} else {
				s.bg = color
			}
			i += 4
		}
	}
}

func (s terminalStyle) css(theme snapshotTheme) string {
	fg, bg := s.fg, s.bg
	if s.invert {
		if fg == "" {
			fg = theme.text
		}
		if bg == "" {
			bg = theme.screen
		}
		fg, bg = bg, fg
	}
	if theme.accessible {
		effectiveFG, effectiveBG := fg, bg
		if effectiveFG == "" {
			effectiveFG = theme.text
		}
		if effectiveBG == "" {
			effectiveBG = theme.screen
		}
		if contrastRatio(effectiveFG, effectiveBG) < 4.5 {
			fg = bestContrast(effectiveBG)
		}
	}
	var css []string
	if fg != "" {
		css = append(css, "color:"+fg)
	}
	if bg != "" {
		css = append(css, "background:"+bg)
	}
	if s.bold {
		css = append(css, "font-weight:700")
	}
	if s.dim && !theme.accessible {
		css = append(css, "opacity:.68")
	}
	if s.italic {
		css = append(css, "font-style:italic")
	}
	var decoration []string
	if s.underline {
		decoration = append(decoration, "underline")
	}
	if s.strike {
		decoration = append(decoration, "line-through")
	}
	if len(decoration) > 0 {
		css = append(css, "text-decoration:"+strings.Join(decoration, " "))
	}
	return strings.Join(css, ";")
}

func xtermColor(n int, theme snapshotTheme) string {
	base := theme.ansi
	if n >= 0 && n < len(base) {
		return base[n]
	}
	if n >= 16 && n <= 231 {
		levels := []int{0, 95, 135, 175, 215, 255}
		n -= 16
		return fmt.Sprintf("#%02x%02x%02x", levels[n/36], levels[(n/6)%6], levels[n%6])
	}
	if n >= 232 && n <= 255 {
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
	return ""
}

func contrastRatio(a, b string) float64 {
	la, oka := luminance(a)
	lb, okb := luminance(b)
	if !oka || !okb {
		return 21
	}
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) (float64, bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, false
	}
	value, err := strconv.ParseUint(hex[1:], 16, 24)
	if err != nil {
		return 0, false
	}
	channels := []float64{float64(value>>16) / 255, float64((value>>8)&0xff) / 255, float64(value&0xff) / 255}
	for i, channel := range channels {
		if channel <= 0.04045 {
			channels[i] = channel / 12.92
		} else {
			channels[i] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2], true
}

func bestContrast(background string) string {
	if contrastRatio("#000000", background) >= contrastRatio("#ffffff", background) {
		return "#000000"
	}
	return "#ffffff"
}

func clamp(n int) int {
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type boundedBuffer struct {
	bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const max = 16 << 10
	original := len(p)
	if b.Len() < max {
		remaining := max - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}
