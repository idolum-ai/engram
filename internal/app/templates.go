package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/templates"
)

func (a *App) expandTypedInput(text string) (string, []string, error) {
	if a.Templates == nil {
		return text, nil, nil
	}
	return a.Templates.Expand(text)
}

func (a *App) recordTemplateUse(names []string, route string, sessionID int) {
	if len(names) == 0 {
		return
	}
	payload := map[string]any{"names": append([]string(nil), names...), "route": route}
	if sessionID > 0 {
		payload["session_id"] = sessionID
	}
	_ = a.audit("template.expand", "ok", payload)
}

func (a *App) handleRememberCommand(ctx context.Context, msg telegram.Message, args string) actionResult {
	if a.Templates == nil {
		return actionResult{Outcome: actionStateFailed, Message: "template store is unavailable"}
	}
	args = strings.TrimSpace(args)
	if args == "" {
		a.reply(ctx, msg, rememberedTemplateList(a.Templates.List()))
		return actionResult{Outcome: actionOK, Message: "listed templates"}
	}
	name, body := splitTemplateDefinition(args)
	if body == "" {
		item, found := a.Templates.Get(name)
		if !found {
			return actionResult{Outcome: actionUserError, Message: "template not found; use /remember to list templates"}
		}
		a.reply(ctx, msg, fmt.Sprintf("{%s}\n\n%s", item.Name, item.Body))
		return actionResult{Outcome: actionOK, Message: "showed template"}
	}
	item, created, err := a.Templates.Put(name, body, time.Now().UTC())
	if err != nil && !templates.PersistenceReachedReplacement(err) {
		return actionResult{Outcome: actionUserError, Message: err.Error()}
	}
	verb := "Updated"
	if created {
		verb = "Remembered"
	}
	if err != nil {
		_ = a.audit("template.remember", "durability_uncertain", map[string]any{"name": item.Name, "error": err.Error()})
	} else {
		_ = a.audit("template.remember", "ok", map[string]any{"name": item.Name, "created": created})
	}
	a.reply(ctx, msg, fmt.Sprintf("%s {%s}.", verb, item.Name))
	return actionResult{Outcome: actionOK, Message: "remembered template"}
}

func (a *App) handleForgetCommand(ctx context.Context, msg telegram.Message, args string) actionResult {
	if a.Templates == nil {
		return actionResult{Outcome: actionStateFailed, Message: "template store is unavailable"}
	}
	name := strings.TrimSpace(args)
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return actionResult{Outcome: actionUserError, Message: "usage: /forget <name>"}
	}
	item, found, err := a.Templates.Forget(name)
	if err != nil && !templates.PersistenceReachedReplacement(err) {
		return actionResult{Outcome: actionStateFailed, Message: err.Error()}
	}
	if !found {
		return actionResult{Outcome: actionUserError, Message: "template not found"}
	}
	if err != nil {
		_ = a.audit("template.forget", "durability_uncertain", map[string]any{"name": item.Name, "error": err.Error()})
	} else {
		_ = a.audit("template.forget", "ok", map[string]any{"name": item.Name})
	}
	a.reply(ctx, msg, fmt.Sprintf("Forgot {%s}.", item.Name))
	return actionResult{Outcome: actionOK, Message: "forgot template"}
}

func (a *App) exportTemplates(ctx context.Context, msg telegram.Message) actionResult {
	if a.Templates == nil {
		return actionResult{Outcome: actionStateFailed, Message: "template store is unavailable"}
	}
	data, err := a.Templates.ExportJSON()
	if err != nil {
		return actionResult{Outcome: actionStateFailed, Message: err.Error()}
	}
	if !a.queueTransfer(func(transferCtx context.Context) {
		a.uploadTemplateExport(transferCtx, msg, data)
	}) {
		return actionResult{Outcome: actionStateFailed, Message: "template export is temporarily unavailable because Engram is stopping or its transfer queue is full"}
	}
	return actionResult{Outcome: actionOK, Message: "queued template export"}
}

func (a *App) uploadTemplateExport(ctx context.Context, msg telegram.Message, data []byte) {
	if err := os.MkdirAll(a.Config.ArtifactDir(), 0o700); err != nil {
		a.reply(ctx, msg, "template export error: "+err.Error())
		_ = a.audit("template.export", "failed", map[string]any{"error": err.Error()})
		return
	}
	file, path, err := createPredictableArtifact(a.Config.ArtifactDir(), "engram-templates-export-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err != nil {
		a.reply(ctx, msg, "template export error: "+err.Error())
		_ = a.audit("template.export", "failed", map[string]any{"error": err.Error()})
		return
	}
	defer os.Remove(path)
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := firstError(writeErr, closeErr); err != nil {
		a.reply(ctx, msg, "template export error: "+err.Error())
		_ = a.audit("template.export", "failed", map[string]any{"error": err.Error()})
		return
	}
	if _, err := a.Telegram.SendDocumentNamed(ctx, msg.Chat.ID, path, "engram-templates.json", "Engram remembered input templates"); err != nil {
		a.reply(ctx, msg, "template export upload error: "+err.Error())
		_ = a.audit("template.export", "failed", map[string]any{"error": err.Error()})
		return
	}
	_ = a.audit("template.export", "ok", map[string]any{"bytes": len(data)})
}

func splitTemplateDefinition(args string) (string, string) {
	for index, r := range args {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return args[:index], strings.TrimSpace(args[index:])
		}
	}
	return args, ""
}

func rememberedTemplateList(items []templates.Template) string {
	if len(items) == 0 {
		return "Remembered templates\n\nNone yet."
	}
	var b strings.Builder
	b.WriteString("Remembered templates\n\n")
	for _, item := range items {
		fmt.Fprintf(&b, "{%s}\n", item.Name)
	}
	b.WriteString("\nUse /remember <name> to inspect one.")
	return strings.TrimSpace(b.String())
}
