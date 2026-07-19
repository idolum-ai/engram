package terminalshot

import (
	"context"
	"fmt"
	"html"
	"image"
	"image/png"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

func TestRenderHTMLBoundsAndEscapesSnapshotStatus(t *testing.T) {
	t.Parallel()
	input := Input{
		ANSI: "ready", Title: "build", Target: "[1]", CWD: "/tmp",
		Columns: 80, VisibleRows: 37, BufferRows: 1,
		Status: strings.Repeat("x", 30) + " <unsafe>",
	}
	page := RenderHTML(input, "contrast-dark")
	want := strings.Repeat("x", footerStatusMaxCells-1) + "…"
	for _, text := range []string{`class="provenance"`, `class="status"`, `class="dimensions"`, want} {
		if !strings.Contains(page, text) {
			t.Fatalf("status footer missing %q: %s", text, page)
		}
	}
	if strings.Contains(page, "<unsafe>") || strings.Contains(page, "&lt;unsafe&gt;") {
		t.Fatalf("truncated status retained unsafe tail: %s", page)
	}
	if got := snapshotFooterStatusCellBudget(input); got != footerStatusMaxCells {
		t.Fatalf("status budget = %d, want %d", got, footerStatusMaxCells)
	}
}

func TestSnapshotStatusBudgetBelongsToSupportedLayout(t *testing.T) {
	t.Parallel()
	for _, input := range []Input{
		{Columns: 80, VisibleRows: 37, BufferRows: 1},
		{Columns: 289, VisibleRows: 162, BufferRows: 1},
		{Columns: 200, VisibleRows: 60, BufferRows: 1, Compact: true},
	} {
		if got := snapshotFooterStatusCellBudget(input); got != footerStatusMaxCells {
			t.Errorf("status budget for %#v = %d, want %d", input, got, footerStatusMaxCells)
		}
	}
}

func TestTruncateTerminalCellsCountsWideAndCombiningRunes(t *testing.T) {
	t.Parallel()
	if got := truncateTerminalCells("disk 47G free", 24); got != "disk 47G free" {
		t.Fatalf("short status = %q", got)
	}
	if got := truncateTerminalCells("磁盘空间还剩很多", 8); got != "磁盘空…" {
		t.Fatalf("wide status = %q", got)
	}
	if got := truncateTerminalCells("e\u0301e\u0301e\u0301", 2); got != "e\u0301…" {
		t.Fatalf("combining status = %q", got)
	}
}

func TestRenderHTMLOmitsEmptySnapshotStatusSlot(t *testing.T) {
	t.Parallel()
	page := RenderHTML(Input{ANSI: "ready", Columns: 80, VisibleRows: 37, BufferRows: 1}, "contrast-dark")
	if strings.Contains(page, `<span class="status">`) {
		t.Fatalf("empty status reserved a footer element: %s", page)
	}
}

func TestRenderHTMLWrapsWidePanesWithoutClipping(t *testing.T) {
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
		"width:100ch",
		"font:7.00px/9.80px",
		"white-space:pre-wrap",
		"overflow-wrap:anywhere",
		"64-row bounded frame",
		"full 289 columns · wrapped at 100 · 162 visible rows",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("wide-pane HTML missing %q: %s", want, page)
		}
	}
}

func TestRenderedColumnsReportsSoftWrapBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		columns int
		want    int
	}{
		{columns: 80, want: 80},
		{columns: 96, want: 96},
		{columns: 97, want: 97},
		{columns: 100, want: 100},
		{columns: 101, want: 100},
		{columns: 289, want: 100},
	} {
		if got := RenderedColumns(test.columns); got != test.want {
			t.Errorf("RenderedColumns(%d) = %d, want %d", test.columns, got, test.want)
		}
	}
}

func TestWideSnapshotDimensionsPreserveAllColumnsWithinTelegramPhotoBounds(t *testing.T) {
	t.Parallel()
	for _, columns := range []int{101, 289, 400} {
		ansi := strings.Repeat(strings.Repeat("x", columns)+"\n", 64)
		input := Input{ANSI: ansi, Columns: columns, VisibleRows: 162, BufferRows: 64}
		width, height := renderWidth(input)*PixelRatio, renderHeight(input)*PixelRatio
		if width+height > 10000 || width > height*20 || height > width*20 {
			t.Fatalf("%d-column snapshot dimensions %dx%d exceed Telegram photo bounds", columns, width, height)
		}
	}
	input := Input{ANSI: strings.Repeat(strings.Repeat("x", 289)+"\n", 64), Columns: 289, VisibleRows: 162, BufferRows: 64}
	if got, want := renderWidth(input), 446; got != want {
		t.Fatalf("wide render width = %d, want %d", got, want)
	}
	if got, want := renderHeight(input), 1958; got != want {
		t.Fatalf("wide render height = %d, want %d", got, want)
	}
	sparse := Input{ANSI: "short line\n", Columns: 289, VisibleRows: 162, BufferRows: 64}
	if got := renderHeight(sparse); got != LogicalHeight {
		t.Fatalf("sparse wide render height = %d, want minimum %d", got, LogicalHeight)
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

func TestCompactEvidenceHTMLHighlightsOnlySelectedRows(t *testing.T) {
	input := Input{
		ANSI: "context\ncritical result\nnext step", Title: "build", Target: "[3]", CWD: "/tmp",
		Columns: 71, VisibleRows: 37, BufferRows: 3, Compact: true, HighlightRows: []int{1},
	}
	page := RenderHTML(input, "contrast-dark")
	if strings.Count(page, `class="evidence-mark"`) != 1 || !strings.Contains(page, `top:23.2px;height:13.2px`) || !strings.Contains(page, ".evidence-mark{position:absolute;left:8px") || !strings.Contains(page, "quoted terminal text") {
		t.Fatalf("compact evidence HTML = %s", page)
	}
	if got := renderHeight(input); got != 180 {
		t.Fatalf("compact render height = %d, want 180", got)
	}
}

func TestCompactEvidencePreservesFittingPhysicalRows(t *testing.T) {
	t.Parallel()
	const columns = 80
	input := Input{
		ANSI: strings.Repeat("─", columns), Title: "codex", Target: "[8]", CWD: "/Users/daniel",
		Columns: columns, VisibleRows: 24, BufferRows: 1, Compact: true, HighlightRows: []int{0}, Footer: "changed terminal region",
	}
	page := RenderHTML(input, "terminal")

	if !strings.Contains(page, "width:80ch") {
		t.Errorf("compact 80-column pane did not retain its physical width")
	}
	if !strings.Contains(page, "tab-size:8;white-space:pre;overflow-wrap:normal;word-break:normal") {
		t.Errorf("compact 80-column pane permits synthetic line wrapping")
	}
	if !strings.Contains(page, `top:10.0px;height:13.2px`) {
		t.Errorf("single highlighted physical row expanded into multiple visual rows")
	}
	if strings.Contains(page, ">80x24 visible</span>") {
		t.Errorf("compact evidence labels the source dimensions as if the full viewport were rendered")
	}
}

func TestReadableCompactTerminalLayoutPreservesRowsThroughMobileWidthLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		columns       int
		renderColumns int
		fontSize      float64
	}{
		{columns: 71, renderColumns: 71, fontSize: maxTerminalFont},
		{columns: 80, renderColumns: 80, fontSize: terminalWidth / (80 * terminalCharRatio)},
		{columns: 96, renderColumns: 96, fontSize: terminalWidth / (96 * terminalCharRatio)},
		{columns: 97, renderColumns: 96, fontSize: minTerminalFont},
		{columns: 200, renderColumns: 96, fontSize: minTerminalFont},
	}
	for _, test := range tests {
		renderColumns, fontSize, lineHeight := readableCompactTerminalLayout(test.columns)
		if renderColumns != test.renderColumns || math.Abs(fontSize-test.fontSize) > 0.001 || lineHeight != compactLineHeight {
			t.Errorf("readableCompactTerminalLayout(%d) = (%d, %.3f, %.1f), want (%d, %.3f, %.1f)", test.columns, renderColumns, fontSize, lineHeight, test.renderColumns, test.fontSize, compactLineHeight)
		}
	}
}

func TestCompactEvidenceWrapsWidePanesAndEscapesFooter(t *testing.T) {
	page := RenderHTML(Input{
		ANSI: "left " + strings.Repeat("x", 150) + " right", Title: "build", Target: "[3]", CWD: "/tmp",
		Columns: 200, VisibleRows: 60, BufferRows: 1, Compact: true, Footer: `<unsafe & footer>`,
	}, "terminal")
	for _, want := range []string{"width:96ch", "font:7.00px/13.2px", "white-space:pre-wrap", "overflow-wrap:anywhere", "word-break:break-all", "left ", " right", "&lt;unsafe &amp; footer&gt;", "200 cols · wraps at 96"} {
		if !strings.Contains(page, want) {
			t.Fatalf("wide compact HTML missing %q: %s", want, page)
		}
	}
	if strings.Contains(page, "translateX") {
		t.Fatal("compact evidence retained horizontal panning")
	}
	if strings.Contains(page, `<unsafe & footer>`) {
		t.Fatal("footer became executable HTML")
	}
}

func TestCompactEvidenceWrappedRowsExpandImageAndHighlights(t *testing.T) {
	rows := make([]string, 18)
	for index := range rows {
		rows[index] = strings.Repeat(string(rune('a'+index%26)), 400)
	}
	input := Input{
		ANSI: strings.Join(rows, "\n"), Title: "wide evidence", Target: "[8]", CWD: "/tmp",
		Columns: 400, VisibleRows: 120, BufferRows: len(rows), Compact: true, HighlightRows: []int{0, 1}, Footer: "wrapped evidence",
	}
	page := RenderHTML(input, "contrast-dark")
	for _, want := range []string{
		`top:10.0px;height:66.0px`,
		`top:76.0px;height:66.0px`,
		"width:96ch",
		"white-space:pre-wrap",
		"word-break:break-all",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("wrapped compact evidence missing %q: %s", want, page)
		}
	}
	width, height := renderWidth(input)*PixelRatio, renderHeight(input)*PixelRatio
	if height <= LogicalHeight*PixelRatio {
		t.Fatalf("wrapped compact height = %d, want greater than fixed portrait height %d", height, LogicalHeight*PixelRatio)
	}
	if width+height > 10000 || width > height*20 || height > width*20 {
		t.Fatalf("wrapped compact dimensions %dx%d exceed Telegram photo bounds", width, height)
	}
}

func TestCompactEvidenceAppliesContrastFloorToTerminalTheme(t *testing.T) {
	page := RenderHTML(Input{
		ANSI: "\x1b[2;38;2;50;50;50mdim dark text\x1b[0m", Columns: 71, VisibleRows: 37, BufferRows: 1, Compact: true,
	}, "terminal")
	if strings.Contains(page, "color:#323232") || strings.Contains(page, "opacity:.68") {
		t.Fatalf("compact terminal theme retained inaccessible styling: %s", page)
	}
}

func TestCompactEvidenceHTMLCanRenderQuietGuidedFrame(t *testing.T) {
	input := Input{
		ANSI: " ", Title: "build", Target: "[3]", CWD: "/tmp",
		Columns: 71, VisibleRows: 37, BufferRows: 1, Compact: true, Footer: "guided view",
	}
	page := RenderHTML(input, "contrast-dark")
	if !strings.Contains(page, "guided view") || strings.Contains(page, "No verified") {
		t.Fatalf("quiet guided HTML = %s", page)
	}
}

func TestOSCTerminatorsPreserveFollowingRenderedTextAndRowCounts(t *testing.T) {
	t.Parallel()
	for name, terminator := range map[string]string{
		"bel":        "\a",
		"escape st":  "\x1b\\",
		"raw c1 st":  string([]byte{0x9c}),
		"utf8 c1 st": "\u009c",
	} {
		t.Run(name, func(t *testing.T) {
			ansi := "before \x1b]8;;file:///tmp/report.txt" + terminator + "AFTER-SENTINEL"
			page := RenderHTML(Input{ANSI: ansi, Columns: 71, VisibleRows: 1, BufferRows: 1}, "contrast-dark")
			if !strings.Contains(page, "before ") || !strings.Contains(page, "AFTER-SENTINEL") || strings.Contains(page, "file:///tmp/report.txt") {
				t.Fatalf("rendered OSC sequence incorrectly: %s", page)
			}
			if got := snapshotRowVisualCounts(ansi, 1, 71); !reflect.DeepEqual(got, []int{1}) {
				t.Fatalf("visual row counts = %#v, want [1]", got)
			}
		})
	}
	unicodeTarget := "file:///tmp/М-report.txt"
	ansi := "\x1b]8;;" + unicodeTarget + "\x1b\\UNICODE-LABEL"
	page := RenderHTML(Input{ANSI: ansi, Columns: 71, VisibleRows: 1, BufferRows: 1}, "contrast-dark")
	if !strings.Contains(page, "UNICODE-LABEL") || strings.Contains(page, unicodeTarget) || strings.Contains(page, "-report.txt") {
		t.Fatalf("multibyte OSC target leaked or hid its label: %s", page)
	}
	if got := snapshotRowVisualCounts(ansi, 1, 71); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("multibyte OSC visual row counts = %#v, want [1]", got)
	}
}

func TestSupportedScreenshotSurfacesContainTheirVisibleAreas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		input      Input
		width      int
		height     int
		bodyLayout string
		font       string
		footer     string
	}{
		{
			name: "narrow full snapshot",
			input: Input{ANSI: "NARROW BODY", Title: "narrow", Target: "[1]", CWD: "/a/very/long/current/working/directory/that/must/stay/in/the/header",
				Columns: 80, VisibleRows: 37, BufferRows: 1, Footer: "NARROW FOOTER"},
			width: LogicalWidth, height: LogicalHeight, bodyLayout: "white-space:pre", font: "font:8.43px/11.80px", footer: "1-row bounded frame · 80x37 visible",
		},
		{
			name: "wide full snapshot",
			input: Input{ANSI: "WIDE BODY", Title: "wide", Target: "[2]", CWD: "/a/very/long/current/working/directory/that/must/stay/in/the/header",
				Columns: 289, VisibleRows: 162, BufferRows: 1, Footer: "WIDE FOOTER"},
			width: 446, height: LogicalHeight, bodyLayout: "white-space:pre-wrap", font: "font:7.00px/9.80px", footer: "1-row bounded frame · full 289 columns · wrapped at 100 · 162 visible rows",
		},
		{
			name: "compact evidence",
			input: Input{ANSI: "COMPACT BODY", Title: "evidence", Target: "[3]", CWD: "/a/very/long/current/working/directory/that/must/stay/in/the/header",
				Columns: 200, VisibleRows: 60, BufferRows: 1, Compact: true, HighlightRows: []int{0}, Footer: "COMPACT FOOTER"},
			width: LogicalWidth, height: 180, bodyLayout: "white-space:pre-wrap", font: "font:7.00px/13.2px", footer: "COMPACT FOOTER",
		},
		{
			name: "quiet guided frame",
			input: Input{ANSI: " ", Title: "quiet", Target: "[4]", CWD: "/a/very/long/current/working/directory/that/must/stay/in/the/header",
				Columns: 71, VisibleRows: 37, BufferRows: 1, Compact: true, Footer: "QUIET FOOTER"},
			width: LogicalWidth, height: 180, bodyLayout: "white-space:pre", font: "font:9.40px/13.2px", footer: "QUIET FOOTER",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := RenderHTML(test.input, "contrast-dark")
			for _, want := range []string{
				fmt.Sprintf(".window{width:%dpx;height:%dpx", test.width, test.height),
				".bar{width:100%;height:44px", "overflow:hidden;border-bottom:1px", ".screen{position:relative;width:100%;height:calc(100% - 66px)",
				".foot{width:100%;height:22px", ".foot span{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}",
				test.bodyLayout, test.font, html.EscapeString(test.input.Title), html.EscapeString(test.input.Target), html.EscapeString(test.input.CWD),
				html.EscapeString(test.footer), html.EscapeString(strings.TrimSpace(test.input.ANSI)),
			} {
				if !strings.Contains(page, want) {
					t.Fatalf("rendered surface missing %q: %s", want, page)
				}
			}
			if strings.Contains(page, "100vw") || strings.Contains(page, "100vh") {
				t.Fatalf("rendered surface depends on browser-clamped viewport units: %s", page)
			}
		})
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

func TestBrowserPathDarwinRequiresDedicatedHeadlessBrowserByDefault(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "google-chrome"))
	t.Setenv("PATH", dir)

	if browser, err := browserPathForOS("", "darwin"); err == nil || browser != "" || !strings.Contains(err.Error(), "dedicated headless") {
		t.Fatalf("desktop Chrome was auto-selected on macOS: browser=%q err=%v", browser, err)
	}

	headless := filepath.Join(dir, "chrome-headless-shell")
	writeExecutable(t, headless)
	if browser, err := browserPathForOS("", "darwin"); err != nil || browser != headless {
		t.Fatalf("dedicated headless browser was not selected on macOS: browser=%q err=%v", browser, err)
	}
}

func TestBrowserPathDarwinAllowsExplicitDesktopBrowser(t *testing.T) {
	desktop := filepath.Join(t.TempDir(), "Google Chrome")
	writeExecutable(t, desktop)
	if browser, err := browserPathForOS(desktop, "darwin"); err != nil || browser != desktop {
		t.Fatalf("explicit desktop browser was not honored: browser=%q err=%v", browser, err)
	}
}

func TestBrowserPathLinuxKeepsChromiumCompatibleFallback(t *testing.T) {
	dir := t.TempDir()
	chrome := filepath.Join(dir, "google-chrome")
	writeExecutable(t, chrome)
	t.Setenv("PATH", dir)
	if browser, err := browserPathForOS("", "linux"); err != nil || browser != chrome {
		t.Fatalf("Linux Chrome fallback was not selected: browser=%q err=%v", browser, err)
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

func TestLiveChromiumSupportedSurfaceAreas(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_SNAPSHOT") != "1" {
		t.Skip("set ENGRAM_LIVE_SNAPSHOT=1 to run the local Chromium render")
	}
	tests := []struct {
		name      string
		input     Input
		bodyText  bool
		highlight bool
	}{
		{
			name: "narrow full snapshot", bodyText: true,
			input: Input{ANSI: "NARROW BODY", Title: "narrow surface", Target: "[1]", CWD: "/a/very/long/current/working/directory/that/must/be/contained/in/the/header",
				Columns: 80, VisibleRows: 37, BufferRows: 1, Status: "disk 47G free"},
		},
		{
			name: "wide full snapshot", bodyText: true,
			input: Input{ANSI: "WIDE BODY", Title: "wide surface", Target: "[2]", CWD: "/a/very/long/current/working/directory/that/must/be/contained/in/the/header",
				Columns: 289, VisibleRows: 162, BufferRows: 1, Status: "disk 47G free"},
		},
		{
			name: "compact evidence", bodyText: true, highlight: true,
			input: Input{ANSI: strings.Repeat(" ", 100) + "COMPACT BODY", Title: "compact surface", Target: "[3]", CWD: "/a/very/long/current/working/directory/that/must/be/contained/in/the/header",
				Columns: 200, VisibleRows: 60, BufferRows: 1, Compact: true, HighlightRows: []int{0}, Footer: "compact evidence footer", Status: "disk 47G free"},
		},
		{
			name: "quiet guided frame",
			input: Input{ANSI: " ", Title: "quiet surface", Target: "[4]", CWD: "/a/very/long/current/working/directory/that/must/be/contained/in/the/header",
				Columns: 71, VisibleRows: 37, BufferRows: 1, Compact: true, Footer: "quiet guided footer", Status: "disk 47G free"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			renderer := New(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"), "contrast-dark")
			path, err := renderer.Render(context.Background(), test.input, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(f)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			width, height := renderWidth(test.input), renderHeight(test.input)
			if img.Bounds().Dx() != width*PixelRatio || img.Bounds().Dy() != height*PixelRatio {
				t.Fatalf("surface dimensions = %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), width*PixelRatio, height*PixelRatio)
			}
			assertVisibleInk(t, img, image.Rect(8, 8, width/2, 38), [3]uint8{0x10, 0x10, 0x10}, "header title")
			assertVisibleInk(t, img, image.Rect(width/2, 8, width-8, 38), [3]uint8{0x10, 0x10, 0x10}, "header location")
			assertVisibleInk(t, img, image.Rect(8, height-18, width/2, height-3), [3]uint8{0x08, 0x08, 0x08}, "footer provenance")
			assertVisibleInk(t, img, image.Rect(width/2+30, height-18, width-80, height-3), [3]uint8{0x08, 0x08, 0x08}, "footer status")
			assertVisibleInk(t, img, image.Rect(width/2, height-18, width-8, height-3), [3]uint8{0x08, 0x08, 0x08}, "footer dimensions")
			if test.bodyText {
				assertVisibleInk(t, img, image.Rect(8, 50, width-8, min(height-24, 110)), [3]uint8{0x00, 0x00, 0x00}, "terminal body")
			}
			if test.highlight && !regionContainsYellow(img, image.Rect(0, 44, 24, min(height-22, 110))) {
				t.Fatal("compact evidence highlight is not visible")
			}
		})
	}
}

func assertVisibleInk(t *testing.T, img image.Image, logical image.Rectangle, background [3]uint8, area string) {
	t.Helper()
	ink := 0
	for y := logical.Min.Y * PixelRatio; y < logical.Max.Y*PixelRatio; y++ {
		for x := logical.Min.X * PixelRatio; x < logical.Max.X*PixelRatio; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if channelDifference(uint8(r>>8), background[0]) > 24 || channelDifference(uint8(g>>8), background[1]) > 24 || channelDifference(uint8(b>>8), background[2]) > 24 {
				ink++
			}
		}
	}
	if ink < 24 {
		t.Fatalf("%s has %d visible foreground pixels", area, ink)
	}
}

func regionContainsYellow(img image.Image, logical image.Rectangle) bool {
	for y := logical.Min.Y * PixelRatio; y < logical.Max.Y*PixelRatio; y++ {
		for x := logical.Min.X * PixelRatio; x < logical.Max.X*PixelRatio; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r > 0xd000 && g > 0xb000 && b < 0x9000 {
				return true
			}
		}
	}
	return false
}

func channelDifference(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestLiveChromiumCompactWrappedRender(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_SNAPSHOT") != "1" {
		t.Skip("set ENGRAM_LIVE_SNAPSHOT=1 to run the local Chromium render")
	}
	renderer := New(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"), "contrast-dark")
	path, err := renderer.Render(context.Background(), Input{
		ANSI: strings.Repeat(" ", 100) + "VISIBLE WRAPPED CONTENT", Title: "compact wrap", Target: "[5]", CWD: "/tmp",
		Columns: 200, VisibleRows: 60, BufferRows: 1, Compact: true,
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
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	visiblePixels := 0
	for y := 52 * PixelRatio; y < 96*PixelRatio; y++ {
		for x := 8 * PixelRatio; x < 180*PixelRatio; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r > 0x4000 || g > 0x4000 || b > 0x4000 {
				visiblePixels++
			}
		}
	}
	if visiblePixels == 0 {
		t.Fatal("wrapped compact evidence rendered an empty terminal body")
	}
}

func TestLiveChromiumWideRender(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_SNAPSHOT") != "1" {
		t.Skip("set ENGRAM_LIVE_SNAPSHOT=1 to run the local Chromium render")
	}
	const columns = 289
	rows := make([]string, 64)
	for index := range rows {
		rows[index] = strings.Repeat("x", columns)
	}
	// Only the final soft-wrapped fragment is cyan. A check that accidentally
	// reaches the preceding line can no longer hide bottom-edge clipping.
	rows[len(rows)-1] = strings.Repeat("x", 200) + "\x1b[36m" + strings.Repeat("Z", 89) + "\x1b[0m"
	ansi := strings.Join(rows, "\n")
	input := Input{
		ANSI: ansi, Title: "wide live render", Target: "[7]", CWD: "/tmp",
		Columns: columns, VisibleRows: 162, BufferRows: 64,
	}
	renderer := New(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"), "contrast-dark")
	path, err := renderer.Render(context.Background(), input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if wantWidth, wantHeight := renderWidth(input)*PixelRatio, renderHeight(input)*PixelRatio; img.Bounds().Dx() != wantWidth || img.Bounds().Dy() != wantHeight {
		t.Fatalf("wide snapshot size = %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), wantWidth, wantHeight)
	}
	logicalHeight := renderHeight(input)
	if !regionContainsCyan(img, image.Rect(8, logicalHeight-32, renderWidth(input)-8, logicalHeight-22)) {
		t.Fatal("final wrapped terminal row is not visible immediately above the footer")
	}
}

func regionContainsCyan(img image.Image, logical image.Rectangle) bool {
	for y := logical.Min.Y * PixelRatio; y < logical.Max.Y*PixelRatio; y++ {
		for x := logical.Min.X * PixelRatio; x < logical.Max.X*PixelRatio; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if b > 0xc000 && g > 0x9000 && r < 0xa000 {
				return true
			}
		}
	}
	return false
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

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
