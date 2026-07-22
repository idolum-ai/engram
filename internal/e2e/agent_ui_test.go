package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentui"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

const agentUIReply = "ENGRAM_MOCK_REPLY semantic rendering is ready."
const fakeClaudeKey = "sk-ant-api03-engram-fixture-not-a-real-credential-000000000000000000000000000000000000"

type agentUIDriver struct {
	name       string
	envKey     string
	lookupName string
	model      string
	configure  func(*testing.T, string, string, string) ([]string, []string)
}

type agentUIEvidence struct {
	Driver                  string           `json:"driver"`
	Version                 string           `json:"version"`
	Platform                string           `json:"platform"`
	RequestPaths            []string         `json:"request_paths"`
	BlockedExternalRequests []string         `json:"blocked_external_requests,omitempty"`
	Active                  agentui.Analysis `json:"active"`
	Idle                    agentui.Analysis `json:"idle"`
}

type agentUIManifest struct {
	Suite   string   `json:"suite"`
	Status  string   `json:"status"`
	Passed  []string `json:"passed"`
	Missing []string `json:"missing,omitempty"`
}

func TestHermeticAgentUISemantics(t *testing.T) {
	if os.Getenv("ENGRAM_AGENT_UI_E2E") != "1" {
		t.Skip("set ENGRAM_AGENT_UI_E2E=1 to run real agent CLIs against loopback model endpoints")
	}
	artifactDir := requiredAbsolutePath(t, "ENGRAM_AGENT_UI_E2E_ARTIFACT_DIR")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmuxBinary := requiredExecutable(t, "ENGRAM_E2E_TMUX", "tmux")
	drivers := agentUIDrivers()
	ran := 0
	passed := make([]string, 0, len(drivers))
	missing := make([]string, 0)
	for _, driver := range drivers {
		driver := driver
		binary, available := optionalAgentBinary(driver)
		if !available {
			missing = append(missing, driver.name)
			t.Run(driver.name, func(t *testing.T) {
				t.Skipf("%s is unavailable; install it or set %s", driver.lookupName, driver.envKey)
			})
			continue
		}
		ran++
		if t.Run(driver.name, func(t *testing.T) {
			runAgentUIDriver(t, artifactDir, tmuxBinary, binary, driver)
		}) {
			passed = append(passed, driver.name)
		}
	}
	status := "passed"
	if len(passed) != ran || os.Getenv("ENGRAM_AGENT_UI_REQUIRE_ALL") == "1" && len(missing) != 0 {
		status = "failed"
	}
	writeAgentUIManifest(t, artifactDir, agentUIManifest{Suite: "agent-ui", Status: status, Passed: passed, Missing: missing})
	if ran == 0 {
		t.Fatal("no agent CLI driver ran")
	}
	if os.Getenv("ENGRAM_AGENT_UI_REQUIRE_ALL") == "1" && len(missing) != 0 {
		t.Fatalf("required agent CLI drivers are unavailable: %s", strings.Join(missing, ", "))
	}
}

func agentUIDrivers() []agentUIDriver {
	return []agentUIDriver{
		{name: "codex", envKey: "ENGRAM_AGENT_UI_CODEX", lookupName: "codex", model: "gpt-5.6-sol", configure: configureCodexDriver},
		{name: "claude", envKey: "ENGRAM_AGENT_UI_CLAUDE", lookupName: "claude", model: "claude-sonnet-4-6", configure: configureClaudeDriver},
		{name: "opencode", envKey: "ENGRAM_AGENT_UI_OPENCODE", lookupName: "opencode", model: "gpt-5.6-sol", configure: configureOpenCodeDriver},
	}
}

func optionalAgentBinary(driver agentUIDriver) (string, bool) {
	value := strings.TrimSpace(os.Getenv(driver.envKey))
	if value == "" {
		value = driver.lookupName
	}
	path, err := exec.LookPath(value)
	if err != nil {
		return "", false
	}
	absolute, err := filepath.Abs(path)
	return absolute, err == nil
}

func runAgentUIDriver(t *testing.T, artifactDir, tmuxBinary, binary string, driver agentUIDriver) {
	t.Helper()
	mock := newAgentModelMock()
	server := httptest.NewServer(mock)
	defer server.Close()

	root := shortTempDir(t)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	home := privateDir(t, root, "home")
	configHome := privateDir(t, root, "config")
	cacheHome := privateDir(t, root, "cache")
	runtimeDir := privateDir(t, root, "runtime")
	tmuxDir := privateDir(t, root, "tmux")
	binDir := privateDir(t, root, "bin")
	workdir := privateDir(t, root, "work")
	writeTmuxWrapper(t, binDir, tmuxBinary)
	driverEnv, args := driver.configure(t, server.URL, home, workdir)
	env := append(isolatedEnvironment(binDir, home, configHome, cacheHome, runtimeDir, tmuxDir), driverEnv...)
	env[0] = "PATH=" + isolatedAgentPath(binDir, binary)
	env = append(env,
		"COLORTERM=truecolor",
		"HTTP_PROXY="+server.URL,
		"HTTPS_PROXY="+server.URL,
		"ALL_PROXY="+server.URL,
		"NO_PROXY=127.0.0.1,localhost",
		"http_proxy="+server.URL,
		"https_proxy="+server.URL,
		"all_proxy="+server.URL,
		"no_proxy=127.0.0.1,localhost",
	)

	sessionName := "agent-ui-" + driver.name
	command := shellQuote(binary)
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	command += `; driver_status=$?; printf '\nENGRAM_DRIVER_EXIT=%s\n' "$driver_status"; sleep 30`
	if output, err := runPrivateTmux(env, "new-session", "-d", "-x", "100", "-y", "30", "-s", sessionName, "-c", workdir, command); err != nil {
		t.Fatalf("start %s in private tmux: %v: %s", driver.name, err, output)
	}
	t.Cleanup(func() {
		_, _ = runPrivateTmux(env, "kill-server")
	})
	paneID := strings.TrimSpace(mustPrivateTmux(t, env, "display-message", "-p", "-t", sessionName, "#{pane_id}"))

	select {
	case <-mock.started:
	case <-time.After(20 * time.Second):
		frame, _ := captureTmux(env, paneID)
		t.Fatalf("%s never reached the loopback model endpoint; frame:\n%s", driver.name, frame)
	}

	activeCapture := waitForAgentCapture(t, env, paneID, 2*time.Second, func(captureText string) bool {
		analysis := agentui.Analyze(agentui.Observation{Current: agentUIFrame(captureText)})
		return analysis.Applied && analysis.Activity == agentui.ActivityActive && !strings.Contains(captureText, agentUIReply)
	})
	active := agentui.Analyze(agentui.Observation{Current: agentUIFrame(activeCapture)})

	idleCapture := waitForAgentCapture(t, env, paneID, 20*time.Second, func(captureText string) bool {
		return strings.Contains(captureText, agentUIReply)
	})
	idle := agentui.Analyze(agentui.Observation{Current: agentUIFrame(idleCapture), Previous: ptrFrame(agentUIFrame(activeCapture))})

	evidence := agentUIEvidence{
		Driver: driver.name, Version: commandVersion(env, binary, "--version"),
		Platform: runtime.GOOS + "/" + runtime.GOARCH, RequestPaths: mock.paths(),
		BlockedExternalRequests: mock.blockedExternal(), Active: active, Idle: idle,
	}
	writeAgentUIEvidence(t, artifactDir, driver.name, activeCapture, idleCapture, evidence)
	if !idle.Applied || idle.Model != driver.model || idle.Activity != agentui.ActivityIdle || !strings.Contains(idle.Conversation, agentUIReply) {
		t.Fatalf("%s idle semantics were not recognized; evidence: %s", driver.name, filepath.Join(artifactDir, driver.name+"-analysis.json"))
	}
	if !active.Applied || active.Model != driver.model || active.Activity != agentui.ActivityActive {
		t.Fatalf("%s active semantics were not recognized; evidence: %s", driver.name, filepath.Join(artifactDir, driver.name+"-analysis.json"))
	}
	if len(evidence.RequestPaths) == 0 {
		t.Fatalf("%s made no loopback model request", driver.name)
	}
	renderAgentUIEvidence(t, artifactDir, driver.name, idleCapture)
}

func isolatedAgentPath(binDir, binary string) string {
	dirs := []string{binDir, filepath.Dir(binary)}
	for _, runtimeName := range []string{"node", "bun"} {
		if path, err := exec.LookPath(runtimeName); err == nil {
			dir := filepath.Dir(path)
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	dirs = append(dirs, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	return strings.Join(dirs, ":")
}

func configureCodexDriver(t *testing.T, baseURL, home, workdir string) ([]string, []string) {
	t.Helper()
	codexHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`model = "gpt-5.6-sol"
model_provider = "engram-mock"
model_reasoning_effort = "high"
approval_policy = "never"
sandbox_mode = "read-only"
check_for_update_on_startup = false

[model_providers.engram-mock]
name = "Engram Mock"
base_url = %q
env_key = "ENGRAM_MOCK_API_KEY"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 10000

[projects.%q]
trust_level = "trusted"
`, baseURL+"/v1", workdir)
	writeFile(t, filepath.Join(codexHome, "config.toml"), config, 0o600)
	return []string{"CODEX_HOME=" + codexHome, "ENGRAM_MOCK_API_KEY=fixture-token"}, []string{"--strict-config", "Review the hermetic fixture without using tools."}
}

func configureClaudeDriver(t *testing.T, baseURL, home, workdir string) ([]string, []string) {
	t.Helper()
	settingsPath := filepath.Join(home, "claude-settings.json")
	writeFile(t, settingsPath, `{"permissions":{"defaultMode":"bypassPermissions"},"disableAllHooks":true}`, 0o600)
	state := map[string]any{
		"hasCompletedOnboarding":        true,
		"theme":                         "dark",
		"bypassPermissionsModeAccepted": true,
		"customApiKeyResponses":         map[string]any{"approved": []string{fakeClaudeKey[len(fakeClaudeKey)-20:]}, "rejected": []string{}},
		"projects":                      map[string]any{workdir: map[string]any{"hasTrustDialogAccepted": true}},
	}
	contents, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".claude.json"), string(contents), 0o600)
	return []string{
		"ANTHROPIC_BASE_URL=" + baseURL,
		"ANTHROPIC_API_KEY=" + fakeClaudeKey,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
	}, []string{"--model", "claude-sonnet-4-6", "--permission-mode", "bypassPermissions", "--prompt-suggestions", "false", "--settings", settingsPath, "Review the hermetic fixture without using tools."}
}

func configureOpenCodeDriver(_ *testing.T, baseURL, _, _ string) ([]string, []string) {
	config := map[string]any{
		"model": "engram/gpt-5.6-sol", "small_model": "engram/gpt-5.6-sol", "autoupdate": false, "share": "disabled",
		"enabled_providers": []string{"engram"},
		"provider": map[string]any{"engram": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Engram Mock",
			"options": map[string]any{"baseURL": baseURL + "/v1", "apiKey": "fixture-token"},
			"models":  map[string]any{"gpt-5.6-sol": map[string]any{"name": "gpt-5.6-sol", "limit": map[string]int{"context": 128000, "output": 4096}}},
		}},
		"permission": map[string]string{"*": "deny"},
	}
	contents, _ := json.Marshal(config)
	return []string{
		"OPENCODE_CONFIG_CONTENT=" + string(contents),
		"OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
		"OPENCODE_DISABLE_LSP_DOWNLOAD=true",
	}, []string{"--pure", "--model", "engram/gpt-5.6-sol", "--prompt", "Review the hermetic fixture without using tools."}
}

func runPrivateTmux(env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func mustPrivateTmux(t *testing.T, env []string, args ...string) string {
	t.Helper()
	output, err := runPrivateTmux(env, args...)
	if err != nil {
		t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return output
}

func waitForAgentCapture(t *testing.T, env []string, paneID string, timeout time.Duration, accept func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		capture, err := captureTmuxEvidence(env, paneID)
		if err != nil || strings.TrimSpace(capture) == "" {
			if raw, rawErr := captureTmux(env, paneID); rawErr == nil {
				capture, err = raw, nil
			}
		}
		if err == nil {
			last = capture
			if accept(capture) {
				return capture
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for agent capture; last frame:\n%s", last)
	return ""
}

func agentUIFrame(text string) agentui.Frame {
	return agentui.Frame{Text: text, Columns: 100, VisibleRows: 30, AlternateScreen: "on", CopyMode: "off"}
}

func ptrFrame(frame agentui.Frame) *agentui.Frame { return &frame }

func writeAgentUIEvidence(t *testing.T, dir, name, active, idle string, evidence agentUIEvidence) {
	t.Helper()
	writeFile(t, filepath.Join(dir, name+"-active.txt"), active, 0o600)
	writeFile(t, filepath.Join(dir, name+"-idle.txt"), idle, 0o600)
	contents, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-analysis.json"), append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeAgentUIManifest(t *testing.T, dir string, manifest agentUIManifest) {
	t.Helper()
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func renderAgentUIEvidence(t *testing.T, artifactDir, name, text string) {
	t.Helper()
	browser := strings.TrimSpace(os.Getenv("ENGRAM_SNAPSHOT_BROWSER"))
	if browser == "" {
		t.Log("ENGRAM_SNAPSHOT_BROWSER is unset; semantic assertions ran without PNG evidence")
		return
	}
	renderer := terminalshot.New(browser, "contrast-dark")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	path, err := renderer.Render(ctx, terminalshot.Input{ANSI: text, Title: "Engram agent UI fixture", Target: name, CWD: "/workspace", Columns: 100, VisibleRows: 30, BufferRows: 30}, artifactDir)
	if err != nil {
		t.Fatalf("render %s evidence: %v", name, err)
	}
	target := filepath.Join(artifactDir, name+"-idle.png")
	if err := os.Rename(path, target); err != nil {
		t.Fatalf("retain %s render evidence: %v", name, err)
	}
}

type agentModelMock struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	seen    []string
}

func newAgentModelMock() *agentModelMock { return &agentModelMock{started: make(chan struct{})} }

func (m *agentModelMock) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if request.Method == http.MethodConnect {
		path = "CONNECT " + request.Host
	}
	m.mu.Lock()
	m.seen = append(m.seen, path)
	m.mu.Unlock()
	if request.Method == http.MethodConnect {
		http.Error(w, "external network disabled by hermetic harness", http.StatusBadGateway)
		return
	}
	switch {
	case strings.HasSuffix(request.URL.Path, "/responses"):
		m.begin()
		streamCodexResponse(w)
	case strings.HasSuffix(request.URL.Path, "/messages"):
		m.begin()
		streamClaudeResponse(w)
	case strings.HasSuffix(request.URL.Path, "/chat/completions"):
		m.begin()
		streamChatResponse(w)
	case strings.HasSuffix(request.URL.Path, "/models"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"}]}`)
	default:
		http.Error(w, "fixture endpoint not found", http.StatusNotFound)
	}
}

func (m *agentModelMock) begin() { m.once.Do(func() { close(m.started) }) }

func (m *agentModelMock) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func (m *agentModelMock) blockedExternal() []string {
	paths := m.paths()
	blocked := make([]string, 0)
	for _, path := range paths {
		if strings.HasPrefix(path, "CONNECT ") {
			blocked = append(blocked, path)
		}
	}
	return blocked
}

func streamCodexResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_engram"}},
		{"type": "response.output_item.added", "item": map[string]any{"type": "reasoning", "id": "reason_engram", "summary": []any{}}},
	}
	writeSSEEvents(w, events)
	flushAndPause(w)
	writeSSEEvents(w, []map[string]any{
		{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "msg_engram", "content": []map[string]string{{"type": "output_text", "text": agentUIReply}}}},
		{"type": "response.completed", "response": map[string]any{"id": "resp_engram", "usage": map[string]any{"input_tokens": 1, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": 2}}},
	})
}

func writeSSEEvents(w http.ResponseWriter, events []map[string]any) {
	for _, event := range events {
		contents, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], contents)
	}
}

func streamClaudeResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeNamedSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_engram", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
	flushAndPause(w)
	writeNamedSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	writeNamedSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": agentUIReply}})
	writeNamedSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeNamedSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	writeNamedSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeNamedSSE(w http.ResponseWriter, name string, event map[string]any) {
	contents, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, contents)
}

func streamChatResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl_engram\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
	flushAndPause(w)
	contents, _ := json.Marshal(agentUIReply)
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_engram\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", contents)
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl_engram\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func flushAndPause(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	time.Sleep(3 * time.Second)
}
