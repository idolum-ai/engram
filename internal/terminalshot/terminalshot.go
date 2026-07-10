package terminalshot

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"image/png"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	LogicalWidth  = 430
	LogicalHeight = 932
	PixelRatio    = 3
	TargetRows    = 64
	maxInputBytes = 1 << 20
)

type Input struct {
	ANSI        string
	Title       string
	Target      string
	CWD         string
	Columns     int
	VisibleRows int
	BufferRows  int
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
	Runner  CommandRunner
}

func New(browser string) *Renderer {
	return &Renderer{Browser: strings.TrimSpace(browser), Runner: ExecRunner{}}
}

func (r *Renderer) Available() (string, error) {
	if r == nil {
		return "", fmt.Errorf("snapshot renderer is not configured")
	}
	return browserPath(r.Browser)
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
	if _, err := io.WriteString(htmlFile, RenderHTML(input)); err != nil {
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
		fmt.Sprintf("--window-size=%d,%d", LogicalWidth, LogicalHeight),
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
	if pngConfig.Width != LogicalWidth*PixelRatio || pngConfig.Height != LogicalHeight*PixelRatio {
		return "", fmt.Errorf("snapshot browser produced %dx%d PNG, want %dx%d", pngConfig.Width, pngConfig.Height, LogicalWidth*PixelRatio, LogicalHeight*PixelRatio)
	}
	if err := os.Chmod(pngPath, 0o600); err != nil {
		return "", fmt.Errorf("protect snapshot PNG: %w", err)
	}
	keepPNG = true
	return pngPath, nil
}

func RenderHTML(input Input) string {
	bufferRows := input.BufferRows
	if bufferRows <= 0 {
		bufferRows = input.VisibleRows
	}
	fontSize := 9.4
	if fit := 406.0 / (float64(input.Columns) * 0.602); fit < fontSize {
		fontSize = fit
	}
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><style>
:root{color-scheme:dark}*{box-sizing:border-box}html,body{margin:0;width:100%%;height:100%%;overflow:hidden;background:#111418}body{color:#d8dee9;font-synthesis:none}.window{width:100vw;height:100vh;overflow:hidden;background:#111418}.bar{height:44px;display:flex;align-items:center;justify-content:space-between;gap:12px;padding:0 12px;border-bottom:1px solid #30353a;background:#202429}.title{flex:0 1 58%%;min-width:0;overflow:hidden;color:#c7ccd1;font:600 12px/1 system-ui,sans-serif;text-overflow:ellipsis;white-space:nowrap}.location{flex:1;min-width:0;overflow:hidden;color:#858c94;font:11px/1 system-ui,sans-serif;text-align:right;text-overflow:ellipsis;white-space:nowrap}.screen{width:100vw;height:calc(100vh - 66px);padding:10px 12px 0;overflow:hidden;background:#111418}pre{width:%dch;height:%dpx;margin:0;overflow:hidden;color:#d8dee9;background:#111418;font:%.2fpx/13.2px "JetBrains Mono","Cascadia Mono","SFMono-Regular",Menlo,Consolas,"DejaVu Sans Mono",monospace;font-variant-ligatures:none;letter-spacing:0;tab-size:8;white-space:pre}.foot{height:22px;display:flex;align-items:center;justify-content:space-between;gap:24px;padding:0 12px;border-top:1px solid #252a2f;color:#858c94;background:#171a1e;font:9px/1 system-ui,sans-serif}
</style></head><body><main class="window"><header class="bar"><div class="title">%s · tmux %s</div><div class="location">%s</div></header><section class="screen"><pre>%s</pre></section><footer class="foot"><span>last %d buffer rows</span><span>%dx%d visible</span></footer></main></body></html>`,
		input.Columns, bufferRows*14, fontSize,
		html.EscapeString(firstNonEmpty(input.Title, "terminal")), html.EscapeString(input.Target), html.EscapeString(input.CWD), ansiHTML(input.ANSI),
		bufferRows, input.Columns, input.VisibleRows)
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
	return nil
}

func browserPath(configured string) (string, error) {
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
	for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	if runtime.GOOS == "darwin" {
		for _, candidate := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("no Chromium-compatible snapshot browser found")
}

type terminalStyle struct {
	fg, bg                    string
	bold, dim, italic         bool
	underline, strike, invert bool
}

func ansiHTML(input string) string {
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
						applySGR(&current, input[i+2:end])
					}
					i = end + 1
					continue
				}
			}
			if i+1 < len(input) && input[i+1] == ']' {
				i += 2
				for i < len(input) {
					if input[i] == 0x07 {
						i++
						break
					}
					if input[i] == 0x1b && i+1 < len(input) && input[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
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
			if css := current.css(); css != "" {
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

func applySGR(s *terminalStyle, raw string) {
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
			s.fg = xtermColor(n - 30)
		case n == 39:
			s.fg = ""
		case n >= 40 && n <= 47:
			s.bg = xtermColor(n - 40)
		case n == 49:
			s.bg = ""
		case n >= 90 && n <= 97:
			s.fg = xtermColor(n - 90 + 8)
		case n >= 100 && n <= 107:
			s.bg = xtermColor(n - 100 + 8)
		case (n == 38 || n == 48) && i+2 < len(values) && values[i+1] == 5:
			color := xtermColor(values[i+2])
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

func (s terminalStyle) css() string {
	fg, bg := s.fg, s.bg
	if s.invert {
		if fg == "" {
			fg = "#d8dee9"
		}
		if bg == "" {
			bg = "#111418"
		}
		fg, bg = bg, fg
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
	if s.dim {
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

func xtermColor(n int) string {
	base := []string{"#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5", "#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff"}
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
