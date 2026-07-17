package terminalshot

import (
	"context"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderHTMLEscapesTerminalContentAndPreservesANSIStyle(t *testing.T) {
	t.Parallel()
	page := RenderHTML(Input{
		ANSI:        "\x1b[31;1merror <script>alert(1)</script>\x1b[0m\n",
		Title:       "build <unsafe>",
		Target:      "[7]",
		CWD:         "/tmp/<cwd>",
		Columns:     71,
		VisibleRows: 37,
		BufferRows:  64,
	}, "terminal")
	for _, want := range []string{
		"color:#cd3131",
		"font-weight:700",
		"error &lt;script&gt;alert(1)&lt;/script&gt;",
		"build &lt;unsafe&gt;",
		"/tmp/&lt;cwd&gt;",
		"64-row bounded frame",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("rendered HTML missing %q", want)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("terminal content became executable HTML")
	}
}

func TestRenderHTMLKeepsWidePanesReadable(t *testing.T) {
	t.Parallel()
	page := RenderHTML(Input{
		ANSI:        "wide terminal content\n",
		Title:       "codex",
		Target:      "[5]",
		CWD:         "/tmp",
		Columns:     289,
		VisibleRows: 162,
		BufferRows:  64,
	}, "contrast-dark")
	for _, want := range []string{
		"width:96ch",
		"font:7.00px/9.80px",
		"64-row bounded frame",
		"columns 1–96 of 289 · 162 visible rows",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("wide-pane HTML missing %q: %s", want, page)
		}
	}
}

func TestRenderedColumnsClipsOnlyBeyondReadableBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		columns int
		want    int
	}{
		{columns: 80, want: 80},
		{columns: 96, want: 96},
		{columns: 97, want: 96},
		{columns: 289, want: 96},
	} {
		if got := RenderedColumns(test.columns); got != test.want {
			t.Errorf("RenderedColumns(%d) = %d, want %d", test.columns, got, test.want)
		}
	}
}

func TestRenderHTMLAccessibilityThemesCorrectLowContrastText(t *testing.T) {
	t.Parallel()
	input := Input{
		ANSI:        "\x1b[2;38;2;50;50;50mdim dark text\x1b[0m\n",
		Title:       "shell",
		Target:      "[1]",
		CWD:         "/tmp",
		Columns:     71,
		VisibleRows: 37,
		BufferRows:  64,
	}

	terminal := RenderHTML(input, "terminal")
	if !strings.Contains(terminal, "color:#323232") || !strings.Contains(terminal, "opacity:.68") {
		t.Fatalf("terminal theme did not preserve source styling: %s", terminal)
	}

	dark := RenderHTML(input, "contrast-dark")
	for _, want := range []string{"color-scheme:dark", "background:#000000", "color:#ffffff"} {
		if !strings.Contains(dark, want) {
			t.Fatalf("contrast-dark HTML missing %q", want)
		}
	}
	if strings.Contains(dark, "opacity:.68") {
		t.Fatal("contrast-dark retained opacity-based dim text")
	}

	input.ANSI = "\x1b[2;38;2;238;238;238mdim light text\x1b[0m\n"
	light := RenderHTML(input, "contrast-light")
	for _, want := range []string{"color-scheme:light", "background:#ffffff", "color:#000000"} {
		if !strings.Contains(light, want) {
			t.Fatalf("contrast-light HTML missing %q", want)
		}
	}
	if strings.Contains(light, "opacity:.68") {
		t.Fatal("contrast-light retained opacity-based dim text")
	}
}

func TestAccessibleThemePalettesMeetTextContrastFloor(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"contrast-dark", "contrast-light"} {
		theme := snapshotThemeFor(name)
		for i, color := range theme.ansi {
			expected := color
			if contrastRatio(color, theme.screen) < 4.5 {
				expected = bestContrast(theme.screen)
			}
			if css := (terminalStyle{fg: color}).css(theme); !strings.Contains(css, "color:"+expected) {
				t.Fatalf("%s ANSI color %d %s rendered as %q, want %s", name, i, color, css, expected)
			}
			if got := contrastRatio(expected, theme.screen); got < 4.5 {
				t.Fatalf("%s corrected ANSI color %d %s contrast = %.2f", name, i, expected, got)
			}
		}
	}
}

func TestRendererUsesPrivateEphemeralBrowserFiles(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeBrowserRunner{t: t}
	renderer := &Renderer{Browser: executable, Theme: "terminal", Runner: runner}
	dir := t.TempDir()
	png, err := renderer.Render(context.Background(), Input{
		ANSI:        "hello\n",
		Title:       "shell",
		Target:      "[1]",
		CWD:         "/tmp",
		Columns:     71,
		VisibleRows: 37,
		BufferRows:  64,
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(png)
	info, err := os.Stat(png)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("PNG metadata = mode %o size %d", info.Mode().Perm(), info.Size())
	}
	if runner.profile == "" || runner.htmlPath == "" {
		t.Fatalf("browser arguments were not captured: %#v", runner.args)
	}
	if _, err := os.Stat(runner.profile); !os.IsNotExist(err) {
		t.Fatalf("browser profile remained after render: %v", err)
	}
	if _, err := os.Stat(runner.htmlPath); !os.IsNotExist(err) {
		t.Fatalf("snapshot HTML remained after render: %v", err)
	}
	joined := strings.Join(runner.args, " ")
	for _, want := range []string{"--window-size=430,932", "--force-device-scale-factor=3", "--disable-background-networking"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("browser args missing %q: %s", want, joined)
		}
	}
}

func TestRendererRejectsMissingConfiguredBrowser(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing-browser")
	if _, err := (&Renderer{Browser: path, Theme: "terminal"}).Available(); err == nil {
		t.Fatal("missing configured browser was accepted")
	}
}

func TestRendererProbeRequiresPNGCapability(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	working := &fakeBrowserRunner{t: t, expected: "Engram snapshot probe"}
	if browser, err := (&Renderer{Browser: executable, Theme: "terminal", Runner: working}).Probe(context.Background()); err != nil || browser != executable {
		t.Fatalf("working probe browser=%q err=%v", browser, err)
	}
	if _, err := os.Stat(working.profile); !os.IsNotExist(err) {
		t.Fatalf("probe browser profile remained: %v", err)
	}
	if _, err := (&Renderer{Browser: executable, Theme: "terminal", Runner: noOutputBrowserRunner{}}).Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "produced no PNG") {
		t.Fatalf("non-rendering executable probe error = %v", err)
	}
}

func TestLiveChromiumRender(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_SNAPSHOT") != "1" {
		t.Skip("set ENGRAM_LIVE_SNAPSHOT=1 to run the local Chromium render")
	}
	renderer := New(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"), "contrast-dark")
	path, err := renderer.Render(context.Background(), Input{
		ANSI:        "\x1b[32;1mEngram terminal snapshot\x1b[0m\n",
		Title:       "live render",
		Target:      "[1]",
		CWD:         "/tmp",
		Columns:     71,
		VisibleRows: 37,
		BufferRows:  64,
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	config, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != LogicalWidth*PixelRatio || config.Height != LogicalHeight*PixelRatio {
		t.Fatalf("snapshot size = %dx%d", config.Width, config.Height)
	}
}

type fakeBrowserRunner struct {
	t        *testing.T
	expected string
	args     []string
	profile  string
	htmlPath string
}

func (r *fakeBrowserRunner) Run(_ context.Context, _ string, args ...string) error {
	r.args = append([]string(nil), args...)
	var screenshot string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--screenshot="):
			screenshot = strings.TrimPrefix(arg, "--screenshot=")
		case strings.HasPrefix(arg, "--user-data-dir="):
			r.profile = strings.TrimPrefix(arg, "--user-data-dir=")
		case strings.HasPrefix(arg, "file:"):
			parsed, err := url.Parse(arg)
			if err != nil {
				r.t.Fatal(err)
			}
			r.htmlPath = parsed.Path
		}
	}
	if screenshot == "" || r.htmlPath == "" {
		r.t.Fatalf("incomplete browser args: %#v", args)
	}
	page, err := os.ReadFile(r.htmlPath)
	if err != nil {
		r.t.Fatal(err)
	}
	expected := r.expected
	if expected == "" {
		expected = "hello"
	}
	if !strings.Contains(string(page), expected) {
		r.t.Fatalf("snapshot HTML missing capture: %s", page)
	}
	f, err := os.OpenFile(screenshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(f, image.NewRGBA(image.Rect(0, 0, LogicalWidth*PixelRatio, LogicalHeight*PixelRatio)))
	closeErr := f.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

type noOutputBrowserRunner struct{}

func (noOutputBrowserRunner) Run(context.Context, string, ...string) error { return nil }
