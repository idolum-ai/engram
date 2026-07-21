package app

import (
	"context"
	"fmt"
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
