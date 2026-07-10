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
	})
	for _, want := range []string{
		"color:#cd3131",
		"font-weight:700",
		"error &lt;script&gt;alert(1)&lt;/script&gt;",
		"build &lt;unsafe&gt;",
		"/tmp/&lt;cwd&gt;",
		"last 64 buffer rows",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("rendered HTML missing %q", want)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("terminal content became executable HTML")
	}
}

func TestRendererUsesPrivateEphemeralBrowserFiles(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeBrowserRunner{t: t}
	renderer := &Renderer{Browser: executable, Runner: runner}
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
	if _, err := (&Renderer{Browser: path}).Available(); err == nil {
		t.Fatal("missing configured browser was accepted")
	}
}

func TestLiveChromiumRender(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_SNAPSHOT") != "1" {
		t.Skip("set ENGRAM_LIVE_SNAPSHOT=1 to run the local Chromium render")
	}
	renderer := New(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"))
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
	if !strings.Contains(string(page), "hello") {
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
