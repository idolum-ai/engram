package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type evidenceManifest struct {
	Suite              string            `json:"suite"`
	Status             string            `json:"status"`
	Assertions         []string          `json:"assertions"`
	TelegramMethods    map[string]int    `json:"telegram_methods,omitempty"`
	RuntimeVersions    map[string]string `json:"runtime_versions,omitempty"`
	SnapshotArtifact   string            `json:"snapshot_artifact,omitempty"`
	TranscriptArtifact string            `json:"transcript_artifact,omitempty"`
	TextArtifact       string            `json:"text_artifact,omitempty"`
	Failure            string            `json:"failure,omitempty"`
}

func writeEvidence(dir string, snapshot fakeTelegramSnapshot, anchorID int, processLog, terminalText, browser string, assertions []string, versions map[string]string) error {
	anchor, ok := snapshot.Messages[anchorID]
	if !ok || len(anchor.Photo) == 0 {
		return fmt.Errorf("canonical snapshot evidence is unavailable")
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.png"), anchor.Photo, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.txt"), []byte(terminalText), 0o600); err != nil {
		return err
	}
	buttons := make([]string, 0)
	for _, row := range anchor.Markup.InlineKeyboard {
		for _, button := range row {
			buttons = append(buttons, button.Text)
		}
	}
	body := renderTranscript(anchor.Caption, terminalText, buttons)
	htmlPath := filepath.Join(dir, "transcript.html")
	if err := os.WriteFile(htmlPath, []byte(body), 0o600); err != nil {
		return err
	}
	if err := renderTranscriptPNG(browser, htmlPath, filepath.Join(dir, "transcript.png")); err != nil {
		return fmt.Errorf("render transcript evidence: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "process.log"), []byte(processLog), 0o600); err != nil {
		return err
	}
	manifest := evidenceManifest{
		Suite:              "hermetic",
		Status:             "passed",
		Assertions:         append([]string(nil), assertions...),
		TelegramMethods:    methodCounts(snapshot.Calls),
		RuntimeVersions:    versions,
		SnapshotArtifact:   "snapshot.png",
		TranscriptArtifact: "transcript.png",
		TextArtifact:       "snapshot.txt",
	}
	return writeManifest(dir, manifest)
}

func writeFailureEvidence(dir string, assertions []string, processLog, telegramDiagnostic string, versions map[string]string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "process.log"), []byte(processLog), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "telegram.log"), []byte(telegramDiagnostic+"\n"), 0o600); err != nil {
		return err
	}
	return writeManifest(dir, evidenceManifest{
		Suite:           "hermetic",
		Status:          "failed",
		Assertions:      append([]string(nil), assertions...),
		RuntimeVersions: versions,
		Failure:         "golden path did not complete; inspect process.log, telegram.log, and the workflow test log",
	})
}

func writeManifest(dir string, manifest evidenceManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600)
}

func methodCounts(methods []string) map[string]int {
	counts := make(map[string]int)
	for _, method := range methods {
		counts[method]++
	}
	return counts
}

func renderTranscript(caption, terminalText string, buttons []string) string {
	var controls strings.Builder
	for _, label := range buttons {
		fmt.Fprintf(&controls, "<span>%s</span>", html.EscapeString(label))
	}
	captionHTML := html.EscapeString(caption)
	captionHTML = strings.ReplaceAll(captionHTML, "&lt;pre&gt;", "<pre>")
	captionHTML = strings.ReplaceAll(captionHTML, "&lt;/pre&gt;", "</pre>")
	return `<!doctype html>
<meta charset="utf-8">
<style>
  * { box-sizing: border-box; }
  body { margin: 0; background: #101311; color: #f3f0e8; font: 16px/1.45 system-ui, sans-serif; }
  main { width: 430px; min-height: 932px; padding: 30px 18px; background: #d7ddd8; }
  .label { margin: 0 0 10px; color: #36413a; font-size: 13px; font-weight: 700; text-transform: uppercase; }
  .card { overflow: hidden; border: 1px solid #8d9991; border-radius: 8px; background: #f7f7f2; color: #172019; box-shadow: 0 8px 20px rgba(0,0,0,.14); }
  figure { margin: 0; }
  .media { height: 510px; overflow: hidden; background: #050505; }
  img { display: block; width: 100%; height: auto; }
  .caption { padding: 16px; white-space: pre-wrap; word-break: break-word; font: 13px/1.45 ui-monospace, monospace; }
  .caption pre { margin: 0 0 14px; white-space: pre-wrap; font: inherit; }
  .controls { display: flex; flex-wrap: wrap; gap: 7px; padding: 0 16px 16px; }
  .controls span { padding: 7px 10px; border: 1px solid #aab3ad; border-radius: 6px; background: #fff; color: #172019; font-size: 12px; font-weight: 650; }
  details { margin: 0 16px 16px; font-size: 13px; }
  details pre { max-height: 240px; overflow: auto; white-space: pre-wrap; word-break: break-word; }
</style>
<main>
  <p class="label" id="evidence-title">Hermetic golden path</p>
  <section class="card" aria-labelledby="evidence-title">
    <figure>
      <div class="media"><img src="snapshot.png" alt="Terminal showing the completed Engram reply-routing fixture"></div>
      <figcaption class="caption">` + captionHTML + `</figcaption>
    </figure>
    <div class="controls" aria-label="Telegram controls">` + controls.String() + `</div>
    <details>
      <summary>Terminal text alternative</summary>
      <pre>` + html.EscapeString(terminalText) + `</pre>
    </details>
  </section>
</main>
`
}

func renderTranscriptPNG(browser, htmlPath, pngPath string) error {
	if browser == "" {
		return fmt.Errorf("snapshot browser is required")
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, browser,
		"--headless", "--disable-background-networking", "--disable-gpu", "--hide-scrollbars",
		"--no-first-run", "--force-device-scale-factor=2", "--window-size=430,1200",
		"--user-data-dir="+profile, "--screenshot="+pngPath, "file://"+absHTML,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalProcessGroup(cmd.Process, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
	}
	return os.Chmod(pngPath, 0o600)
}
