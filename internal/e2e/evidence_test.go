package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type evidenceManifest struct {
	Suite              string         `json:"suite"`
	Status             string         `json:"status"`
	Assertions         []string       `json:"assertions"`
	TelegramMethods    map[string]int `json:"telegram_methods"`
	SnapshotArtifact   string         `json:"snapshot_artifact"`
	TranscriptArtifact string         `json:"transcript_artifact"`
}

func writeEvidence(t *testing.T, dir string, snapshot fakeTelegramSnapshot, anchorID int, processLog string) {
	t.Helper()
	anchor := snapshot.Messages[anchorID]
	if err := os.WriteFile(filepath.Join(dir, "snapshot.png"), anchor.Photo, 0o600); err != nil {
		t.Fatal(err)
	}
	buttons := make([]string, 0)
	for _, row := range anchor.Markup.InlineKeyboard {
		for _, button := range row {
			buttons = append(buttons, button.Text)
		}
	}
	body := renderTranscript(anchor.Caption, buttons)
	htmlPath := filepath.Join(dir, "transcript.html")
	if err := os.WriteFile(htmlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renderTranscriptPNG(htmlPath, filepath.Join(dir, "transcript.png")); err != nil {
		t.Fatalf("render transcript evidence: %v", err)
	}
	methods := make(map[string]int)
	for _, method := range snapshot.Calls {
		methods[method]++
	}
	manifest := evidenceManifest{
		Suite:  "hermetic",
		Status: "passed",
		Assertions: []string{
			"Telegram message created an isolated tmux window",
			"canonical anchor became a pinned Chromium snapshot",
			"reply input reached the tracked pane",
			"manual refresh edited the canonical anchor",
			"numbered file callback delivered exact bytes",
		},
		TelegramMethods:    methods,
		SnapshotArtifact:   "snapshot.png",
		TranscriptArtifact: "transcript.png",
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(processLog) != "" {
		if err := os.WriteFile(filepath.Join(dir, "process.log"), []byte(processLog), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func renderTranscript(caption string, buttons []string) string {
	var controls strings.Builder
	for _, label := range buttons {
		fmt.Fprintf(&controls, "<span>%s</span>", html.EscapeString(label))
	}
	return `<!doctype html>
<meta charset="utf-8">
<style>
  * { box-sizing: border-box; }
  body { margin: 0; background: #101311; color: #f3f0e8; font: 16px/1.45 system-ui, sans-serif; }
  main { width: 430px; min-height: 932px; padding: 30px 18px; background: #d7ddd8; }
  .label { margin: 0 0 10px; color: #36413a; font-size: 13px; font-weight: 700; text-transform: uppercase; }
  .card { overflow: hidden; border: 1px solid #8d9991; border-radius: 8px; background: #f7f7f2; color: #172019; box-shadow: 0 8px 20px rgba(0,0,0,.14); }
  .media { height: 510px; overflow: hidden; background: #050505; }
  img { display: block; width: 100%; height: auto; }
  .caption { padding: 16px; white-space: pre-wrap; word-break: break-word; font: 13px/1.45 ui-monospace, monospace; }
  .caption pre { margin: 0 0 14px; white-space: pre-wrap; font: inherit; }
  .controls { display: flex; flex-wrap: wrap; gap: 7px; padding: 0 16px 16px; }
  .controls span { padding: 7px 10px; border: 1px solid #aab3ad; border-radius: 6px; background: #fff; color: #172019; font-size: 12px; font-weight: 650; }
</style>
<main>
  <p class="label">Hermetic golden path</p>
  <section class="card">
    <div class="media"><img src="snapshot.png" alt="Rendered Engram terminal snapshot"></div>
    <div class="caption">` + caption + `</div>
    <div class="controls">` + controls.String() + `</div>
  </section>
</main>
`
}

func renderTranscriptPNG(htmlPath, pngPath string) error {
	browser := os.Getenv("ENGRAM_SNAPSHOT_BROWSER")
	if browser == "" {
		return fmt.Errorf("ENGRAM_SNAPSHOT_BROWSER is required")
	}
	profile, err := os.MkdirTemp(filepath.Dir(pngPath), ".transcript-browser-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	absHTML, err := filepath.Abs(htmlPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(browser,
		"--headless", "--disable-background-networking", "--disable-gpu", "--hide-scrollbars",
		"--no-first-run", "--force-device-scale-factor=2", "--window-size=430,1200",
		"--user-data-dir="+profile, "--screenshot="+pngPath, "file://"+absHTML,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
	}
	return os.Chmod(pngPath, 0o600)
}
