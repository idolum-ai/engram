package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Metadata struct {
	Command     string `json:"command"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

type BotCommand struct {
	Command     string
	Description string
}

var registry = []Metadata{
	{Command: "help", Usage: "/help", Description: "Show available commands", Category: "service"},
	{Command: "status", Usage: "/status", Description: "Show service, state, and tmux status", Category: "service"},
	{Command: "mode", Usage: "/mode [guide|snapshot]", Description: "Show or change the live anchor mode", Category: "service"},
	{Command: "logs", Usage: "/logs", Description: "Send service audit logs as an attachment", Category: "service"},
	{Command: "restart", Usage: "/restart", Description: "Restart Engram without closing tmux sessions", Category: "service"},
	{Command: "remember", Usage: "/remember [<name> [text]]", Description: "List, inspect, or save an input template", Category: "input"},
	{Command: "forget", Usage: "/forget <name>", Description: "Forget a saved input template", Category: "input"},
	{Command: "templates", Usage: "/templates", Description: "Download remembered input templates as JSON", Category: "input"},
	{Command: "sessions", Usage: "/sessions", Description: "List active terminal sessions with controls", Category: "session"},
	{Command: "recovery", Usage: "/recovery", Description: "Show a deterministic plan for lost sessions", Category: "session"},
	{Command: "attach", Usage: "/attach <tmux-target>", Description: "Track an existing tmux window or pane", Category: "session"},
	{Command: "new", Usage: "/new <text>", Description: "Create a tmux window and send text as a shell command", Category: "session"},
	{Command: "resume", Usage: "/resume <id> [<codex|claude> <session-uuid>]", Description: "Restore a lost agent session into its existing watch", Category: "session"},
	{Command: "send", Usage: "/send <id> <text>", Description: "Send text as a shell command to a session", Category: "session"},
	{Command: "text", Usage: "/text <id> <text>", Description: "Send literal text without pressing Enter", Category: "session"},
	{Command: "key", Usage: "/key <id> <keys...>", Description: "Send tmux key names to a session", Category: "session"},
	{Command: "rename", Usage: "/rename <id> <name>", Description: "Rename a tracked session", Category: "session"},
	{Command: "cwd", Usage: "/cwd <id>", Description: "Show a session pane's working directory", Category: "session"},
	{Command: "cd", Usage: "/cd <id> <path>", Description: "Change a session pane's working directory", Category: "session"},
	{Command: "watch", Usage: "/watch <id>", Description: "Enable anchor updates for a session", Category: "session"},
	{Command: "unwatch", Usage: "/unwatch <id>", Description: "Disable anchor updates for a session", Category: "session"},
	{Command: "close", Usage: "/close <id>", Description: "Close a tracked tmux window", Category: "session"},
	{Command: "dump", Usage: "/dump <id>", Description: "Send the full tmux scrollback as an attachment", Category: "capture"},
	{Command: "raw", Usage: "/raw <id>", Description: "Send the visible tmux pane as an attachment", Category: "capture"},
	{Command: "attachments", Usage: "/attachments", Description: "List files received from Telegram", Category: "files"},
	{Command: "download", Usage: "/download <absolute-path>", Description: "Upload an absolute local path to Telegram", Category: "files"},
	{Command: "attachment_bypass", Usage: "/attachment_bypass sha256:<hash>", Description: "Authorize one large attachment by SHA-256", Category: "files"},
}

func All() []Metadata {
	out := make([]Metadata, len(registry))
	copy(out, registry)
	return out
}

func Find(command string) (Metadata, bool) {
	command = strings.TrimPrefix(strings.TrimSpace(command), "/")
	for _, meta := range registry {
		if meta.Command == command {
			return meta, true
		}
	}
	return Metadata{}, false
}

func BotCommands() []BotCommand {
	out := make([]BotCommand, 0, len(registry))
	for _, meta := range registry {
		if !validTelegramCommand(meta.Command) {
			continue
		}
		out = append(out, BotCommand{Command: meta.Command, Description: meta.Description})
	}
	return out
}

func JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(All()); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func validTelegramCommand(command string) bool {
	if len(command) == 0 || len(command) > 32 {
		return false
	}
	for _, r := range command {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

func HelpText() string {
	var b strings.Builder
	b.WriteString("Commands\n\n")
	for _, meta := range registry {
		fmt.Fprintf(&b, "%s - %s\n", meta.Usage, meta.Description)
	}
	b.WriteString("\nSession input\n")
	b.WriteString("Reply with //text to send /text to that session; for example, //clear sends /clear.")
	return strings.TrimRight(b.String(), "\n")
}
