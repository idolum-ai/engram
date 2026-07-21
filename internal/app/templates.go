package app

import (
	"fmt"
	"strings"

	"github.com/idolum-ai/engram/internal/templates"
)

func (a *App) prepareTypedInput(text, route string, sessionID int) (string, error) {
	if a.Templates == nil {
		return text, nil
	}
	expanded, names, err := a.Templates.Expand(text)
	if err != nil {
		return "", err
	}
	recordTemplateUse(a, names, route, sessionID)
	return expanded, nil
}

func recordTemplateUse(a *App, names []string, route string, sessionID int) {
	if len(names) == 0 {
		return
	}
	payload := map[string]any{"names": append([]string(nil), names...), "route": route}
	if sessionID > 0 {
		payload["session_id"] = sessionID
	}
	_ = a.audit("template.expand", "prepared", payload)
}

func (a *App) handleRememberCommand(args string) actionResult {
	if a.Templates == nil {
		return actionResult{Outcome: actionStateFailed, Message: "template store is unavailable"}
	}
	name, body, hasBody := splitTemplateDefinition(args)
	if name == "" {
		return actionResult{Outcome: actionOK, Message: rememberedTemplateList(a.Templates.List())}
	}
	if !hasBody {
		item, found := a.Templates.Get(name)
		if !found {
			return actionResult{Outcome: actionUserError, Message: "template not found; use /remember to list templates"}
		}
		return actionResult{Outcome: actionOK, Message: fmt.Sprintf("{engram:%s}\n\n%s", item.Name, item.Body)}
	}
	item, created, err := a.Templates.Put(name, body)
	if err != nil && !templates.PersistenceReachedReplacement(err) {
		outcome := actionStateFailed
		if templates.IsValidationError(err) {
			outcome = actionUserError
		}
		return actionResult{Outcome: outcome, Message: err.Error()}
	}
	verb := "Updated"
	if created {
		verb = "Remembered"
	}
	if err != nil {
		_ = a.audit("template.remember", "durability_uncertain", map[string]any{"name": item.Name, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: fmt.Sprintf("%s {engram:%s}, but disk durability is uncertain: %s", verb, item.Name, err)}
	} else {
		_ = a.audit("template.remember", "ok", map[string]any{"name": item.Name, "created": created})
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("%s {engram:%s}.", verb, item.Name)}
}

func (a *App) handleForgetCommand(args string) actionResult {
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
		return actionResult{Outcome: actionStateFailed, Message: fmt.Sprintf("Forgot {engram:%s} in the running process, but disk durability is uncertain: %s", item.Name, err)}
	} else {
		_ = a.audit("template.forget", "ok", map[string]any{"name": item.Name})
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("Forgot {engram:%s}.", item.Name)}
}

func splitTemplateDefinition(args string) (string, string, bool) {
	args = strings.TrimLeft(args, " \t\r\n")
	for index, r := range args {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			next := index + 1
			if r == '\r' && next < len(args) && args[next] == '\n' {
				next++
			}
			return args[:index], args[next:], true
		}
	}
	return args, "", false
}

func rememberedTemplateList(items []templates.Template) string {
	if len(items) == 0 {
		return "Remembered templates\n\nNone yet."
	}
	var b strings.Builder
	b.WriteString("Remembered templates\n\n")
	for _, item := range items {
		fmt.Fprintf(&b, "{engram:%s}\n", item.Name)
	}
	b.WriteString("\nUse /remember <name> to inspect one.")
	return strings.TrimSpace(b.String())
}
