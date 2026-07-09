package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/commands"
	"github.com/idolum-ai/engram/internal/redact"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) captureFile(ctx context.Context, msg telegram.Message, arg string, full bool) {
	id, ok := parseID(arg)
	if !ok {
		if full {
			a.reply(ctx, msg, "usage: /dump <id>")
		} else {
			a.reply(ctx, msg, "usage: /raw <id>")
		}
		return
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session not found")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.validateSessionPane(tctx, ts); err != nil {
		a.reply(ctx, msg, "session lost; use /sessions to attach the intended pane again")
		return
	}
	var text string
	var err error
	name := "raw"
	if full {
		text, err = a.Tmux.CaptureFull(tctx, ts.TmuxPaneID)
		name = "dump"
	} else {
		text, err = a.Tmux.CaptureVisible(tctx, ts.TmuxPaneID)
	}
	if err != nil {
		a.reply(ctx, msg, "capture error: "+err.Error())
		return
	}
	path := filepath.Join(a.Config.ArtifactDir(), fmt.Sprintf("engram-%s-%d-%s.txt", name, id, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		a.reply(ctx, msg, "write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, fmt.Sprintf("[%d] %s", id, name)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

func (a *App) commandsMetadata(ctx context.Context, msg telegram.Message) {
	data, err := commands.JSON()
	if err != nil {
		a.reply(ctx, msg, "commands error: "+err.Error())
		return
	}
	path := filepath.Join(a.Config.ArtifactDir(), "engram-commands-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		a.reply(ctx, msg, "write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, "Engram command metadata"); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

func (a *App) handleAttachment(ctx context.Context, msg telegram.Message, doc telegram.Document) {
	allowLarge := strings.Contains(msg.Caption, "/attachment-bypass")
	expectedHash := parseBypassHash(msg.Caption)
	if expectedHash == "" {
		if bypass, ok := a.Store.FindAttachmentBypass(msg.Chat.ID, msg.From.ID); ok {
			expectedHash = bypass.SHA256
			allowLarge = true
		}
	}
	if doc.FileSize > a.Config.AttachmentSoftMaxBytes && !allowLarge {
		a.reply(ctx, msg, a.largeAttachmentMessage(doc.FileSize))
		return
	}
	if allowLarge && !validSHA256Hex(expectedHash) {
		a.reply(ctx, msg, "large attachment bypass requires sha256:<hash>")
		return
	}
	file, err := a.Telegram.GetFile(ctx, doc.FileID)
	if err != nil {
		a.reply(ctx, msg, "getFile error: "+err.Error())
		return
	}
	name := safeName(firstNonEmpty(doc.FileName, filepath.Base(file.FilePath), doc.FileID))
	path := filepath.Join(a.Config.AttachmentDir(), time.Now().UTC().Format("20060102T150405Z")+"-"+doc.FileID+"-"+name)
	maxBytes := a.Config.AttachmentSoftMaxBytes
	if allowLarge {
		maxBytes = 0
	}
	n, err := a.Telegram.DownloadFile(ctx, file.FilePath, path, maxBytes)
	if err != nil {
		if strings.Contains(err.Error(), "download exceeded max bytes") {
			a.reply(ctx, msg, a.largeAttachmentMessage(n))
			return
		}
		a.reply(ctx, msg, "download error: "+err.Error())
		return
	}
	sum, err := fileSHA256(path)
	if err != nil {
		a.reply(ctx, msg, "hash error: "+err.Error())
		return
	}
	if allowLarge && expectedHash != "" && !strings.EqualFold(sum, expectedHash) {
		_ = os.Remove(path)
		a.reply(ctx, msg, fmt.Sprintf("attachment hash mismatch\nexpected: %s\nactual: %s", expectedHash, sum))
		return
	}
	if allowLarge && expectedHash != "" {
		if err := a.Store.ConsumeAttachmentBypass(msg.Chat.ID, msg.From.ID, expectedHash); err != nil {
			a.reply(ctx, msg, "state error: "+err.Error())
			return
		}
	}
	if err := a.Store.AddAttachment(state.Attachment{
		TelegramFileID:       doc.FileID,
		TelegramUniqueFileID: doc.FileUniqueID,
		ChatID:               msg.Chat.ID,
		UserID:               msg.From.ID,
		OriginalName:         doc.FileName,
		ContentType:          doc.MimeType,
		SizeBytes:            n,
		SHA256:               sum,
		StoredPath:           path,
		ReceivedAt:           time.Now().UTC(),
		BypassRequested:      allowLarge,
	}); err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	a.reply(ctx, msg, "attachment saved\n"+path)
}

func (a *App) largeAttachmentMessage(size int64) string {
	space := diskFree(a.Config.ArtifactDir())
	return fmt.Sprintf("attachment too large: %d bytes exceeds soft limit %d bytes\navailable /tmp/engram space: %d bytes\nresend with caption `/attachment-bypass sha256:<hash>` to bypass", size, a.Config.AttachmentSoftMaxBytes, space)
}

func (a *App) attachments(ctx context.Context, msg telegram.Message) {
	st := a.Store.Snapshot()
	if len(st.Attachments) == 0 {
		a.reply(ctx, msg, "No attachments.")
		return
	}
	var b strings.Builder
	for _, att := range st.Attachments {
		fmt.Fprintf(&b, "%s  %d bytes\n%s\n", att.ReceivedAt.Format(time.RFC3339), att.SizeBytes, att.StoredPath)
	}
	a.reply(ctx, msg, b.String())
}

func (a *App) download(ctx context.Context, msg telegram.Message, path string) {
	path = strings.TrimSpace(path)
	if _, err := validateDownloadPath(path); err != nil {
		a.reply(ctx, msg, err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, a.Config.TelegramChatID, path, filepath.Base(path)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

func (a *App) logs(ctx context.Context, msg telegram.Message) {
	b, err := tailFile(a.Config.AuditPath(), 200_000)
	if err != nil {
		a.reply(ctx, msg, "log read error: "+err.Error())
		return
	}
	text := redact.Secrets(string(b), a.Config.TelegramBotToken, a.Config.AnthropicAPIKey)
	path := filepath.Join(a.Config.ArtifactDir(), "engram-logs-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		a.reply(ctx, msg, "log write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, "Engram logs"); err != nil {
		a.reply(ctx, msg, "log upload error: "+err.Error())
	}
}

func tailFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("tail size must be positive")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

func safeName(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '-'
		}
		return r
	}, name)
	if name == "." || name == "" {
		return "attachment"
	}
	return name
}

func validateDownloadPath(path string) (os.FileInfo, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("/download requires an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat error: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return info, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func diskFree(path string) uint64 {
	_ = os.MkdirAll(path, 0o700)
	var stat syscallStatfs
	if err := statfs(path, &stat); err != nil {
		return 0
	}
	return stat.Bavail * uint64(stat.Bsize)
}
