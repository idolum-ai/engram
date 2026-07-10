package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/redact"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

const (
	telegramCloudDownloadMax = int64(20 * 1024 * 1024)
	telegramCloudUploadMax   = int64(50 * 1024 * 1024)
	attachmentDiskReserve    = uint64(64 * 1024 * 1024)
)

var errArtifactTooLarge = errors.New("artifact exceeds Telegram cloud upload limit")

func (a *App) captureFile(ctx context.Context, msg telegram.Message, arg string, full bool) actionResult {
	id, ok := parseID(arg)
	if !ok {
		if full {
			a.reply(ctx, msg, "usage: /dump <id>")
		} else {
			a.reply(ctx, msg, "usage: /raw <id>")
		}
		return actionResult{Outcome: actionUserError, Message: "invalid capture session"}
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session not found")
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	kind := "raw"
	if full {
		kind = "dump"
	}
	a.reply(ctx, msg, fmt.Sprintf("preparing [%d] %s...", id, kind))
	if !a.queueTransfer(func(transferCtx context.Context) {
		a.captureSessionFile(transferCtx, msg, ts, full)
	}) {
		a.reply(ctx, msg, "capture unavailable: Engram is stopping or the transfer queue is full")
		return actionResult{Outcome: actionStateFailed, Message: "capture transfer unavailable"}
	}
	return actionResult{Outcome: actionOK, Message: kind + " queued"}
}

func (a *App) captureSessionFile(ctx context.Context, msg telegram.Message, ts state.TerminalSession, full bool) {
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.validateSessionPane(tctx, ts); err != nil {
		a.reply(ctx, msg, "session lost; use /sessions to attach the intended pane again")
		return
	}
	name := "raw"
	path := filepath.Join(a.Config.ArtifactDir(), fmt.Sprintf("engram-%s-%d-%s.txt", name, ts.ID, time.Now().UTC().Format("20060102T150405Z")))
	if full {
		name = "dump"
		path = filepath.Join(a.Config.ArtifactDir(), fmt.Sprintf("engram-%s-%d-%s.txt", name, ts.ID, time.Now().UTC().Format("20060102T150405Z")))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			a.reply(ctx, msg, "write error: "+err.Error())
			return
		}
		dumpErr := a.Tmux.DumpScrollback(tctx, ts.TmuxPaneID, &boundedWriter{Writer: file, Remaining: telegramCloudUploadMax})
		closeErr := file.Close()
		if dumpErr != nil || closeErr != nil {
			_ = os.Remove(path)
			if errors.Is(dumpErr, errArtifactTooLarge) {
				a.reply(ctx, msg, fmt.Sprintf("capture rejected: scrollback exceeds Telegram's %d-byte cloud Bot API limit", telegramCloudUploadMax))
				return
			}
			a.reply(ctx, msg, "capture error: "+firstError(dumpErr, closeErr).Error())
			return
		}
	} else {
		text, err := a.Tmux.CaptureVisibleRaw(tctx, ts.TmuxPaneID)
		if err != nil {
			a.reply(ctx, msg, "capture error: "+err.Error())
			return
		}
		if int64(len(text)) > telegramCloudUploadMax {
			a.reply(ctx, msg, fmt.Sprintf("capture rejected: visible pane exceeds Telegram's %d-byte cloud Bot API limit", telegramCloudUploadMax))
			return
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			a.reply(ctx, msg, "write error: "+err.Error())
			return
		}
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, fmt.Sprintf("[%d] %s", ts.ID, name)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

type boundedWriter struct {
	Writer    io.Writer
	Remaining int64
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if w.Remaining <= 0 {
		return 0, errArtifactTooLarge
	}
	if int64(len(data)) > w.Remaining {
		written, err := w.Writer.Write(data[:w.Remaining])
		w.Remaining -= int64(written)
		if err != nil {
			return written, err
		}
		return written, errArtifactTooLarge
	}
	written, err := w.Writer.Write(data)
	w.Remaining -= int64(written)
	return written, err
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) handleAttachment(ctx context.Context, msg telegram.Message, doc telegram.Document) actionResult {
	allowLarge := strings.Contains(msg.Caption, "/attachment-bypass") || strings.Contains(msg.Caption, "/attachment_bypass")
	expectedHash := parseBypassHash(msg.Caption)
	if expectedHash == "" {
		if bypass, ok := a.Store.FindAttachmentBypass(msg.Chat.ID, msg.From.ID); ok {
			expectedHash = bypass.SHA256
			allowLarge = true
		}
	}
	hardMax := a.attachmentHardMax()
	softMax := minInt64(a.Config.AttachmentSoftMaxBytes, hardMax)
	if doc.FileSize > hardMax {
		a.reply(ctx, msg, fmt.Sprintf("attachment rejected: %d bytes exceeds the hard limit %d bytes; the Telegram cloud Bot API cannot download it", doc.FileSize, hardMax))
		return actionResult{Outcome: actionUserError, Message: "attachment exceeds hard limit"}
	}
	if doc.FileSize > softMax && !allowLarge {
		a.reply(ctx, msg, a.largeAttachmentMessage(doc.FileSize))
		return actionResult{Outcome: actionUserError, Message: "attachment exceeds soft limit"}
	}
	if allowLarge && !validSHA256Hex(expectedHash) {
		a.reply(ctx, msg, "large attachment bypass requires sha256:<hash>")
		return actionResult{Outcome: actionUserError, Message: "invalid attachment bypass"}
	}
	a.reply(ctx, msg, "receiving attachment...")
	if !a.queueTransfer(func(transferCtx context.Context) {
		a.downloadAttachment(transferCtx, msg, doc, allowLarge, expectedHash, softMax, hardMax)
	}) {
		a.reply(ctx, msg, "attachment transfer unavailable: Engram is stopping or the transfer queue is full")
		return actionResult{Outcome: actionStateFailed, Message: "attachment transfer unavailable"}
	}
	return actionResult{Outcome: actionOK, Message: "attachment queued"}
}

func (a *App) downloadAttachment(ctx context.Context, msg telegram.Message, doc telegram.Document, allowLarge bool, expectedHash string, softMax, hardMax int64) {
	file, err := a.Telegram.GetFile(ctx, doc.FileID)
	if err != nil {
		a.reply(ctx, msg, "getFile error: "+err.Error())
		return
	}
	name := safeName(firstNonEmpty(doc.FileName, filepath.Base(file.FilePath), doc.FileID))
	path := filepath.Join(a.Config.AttachmentDir(), time.Now().UTC().Format("20060102T150405Z")+"-"+doc.FileID+"-"+name)
	maxBytes := softMax
	if allowLarge {
		maxBytes = hardMax
	}
	download, err := a.Telegram.DownloadFileHashed(ctx, file.FilePath, path, maxBytes)
	if err != nil {
		if strings.Contains(err.Error(), "download exceeded max bytes") {
			a.reply(ctx, msg, a.largeAttachmentMessage(download.Size))
			return
		}
		a.reply(ctx, msg, "download error: "+err.Error())
		return
	}
	sum := download.SHA256
	if allowLarge && expectedHash != "" && !strings.EqualFold(sum, expectedHash) {
		_ = os.Remove(path)
		a.reply(ctx, msg, fmt.Sprintf("attachment hash mismatch\nexpected: %s\nactual: %s", expectedHash, sum))
		return
	}
	if allowLarge && expectedHash != "" {
		if err := a.Store.ConsumeAttachmentBypass(msg.Chat.ID, msg.From.ID, expectedHash); err != nil {
			_ = os.Remove(path)
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
		SizeBytes:            download.Size,
		SHA256:               sum,
		StoredPath:           path,
		ReceivedAt:           time.Now().UTC(),
		BypassRequested:      allowLarge,
	}); err != nil {
		_ = os.Remove(path)
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	a.reply(ctx, msg, "attachment saved\n"+path)
}

func (a *App) largeAttachmentMessage(size int64) string {
	space := diskFree(a.Config.ArtifactDir())
	return fmt.Sprintf("attachment too large: %d bytes exceeds soft limit %d bytes\nhard limit: %d bytes\navailable /tmp/engram space: %d bytes\nresend with caption `/attachment_bypass sha256:<hash>` to authorize the exact file", size, minInt64(a.Config.AttachmentSoftMaxBytes, a.attachmentHardMax()), a.attachmentHardMax(), space)
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

func (a *App) download(ctx context.Context, msg telegram.Message, path string) actionResult {
	path = strings.TrimSpace(path)
	source, info, err := openDownloadSource(path)
	if err != nil {
		a.reply(ctx, msg, err.Error())
		return actionResult{Outcome: actionUserError, Message: "invalid download path"}
	}
	if info.Size() > telegramCloudUploadMax {
		source.Close()
		a.reply(ctx, msg, fmt.Sprintf("send rejected: %d bytes exceeds Telegram's %d-byte cloud Bot API limit", info.Size(), telegramCloudUploadMax))
		return actionResult{Outcome: actionUserError, Message: "download exceeds upload limit"}
	}
	filename := filepath.Base(path)
	a.reply(ctx, msg, fmt.Sprintf("sending %s (%d bytes)...", filename, info.Size()))
	if !a.queueTransfer(func(transferCtx context.Context) {
		defer source.Close()
		snapshot, err := a.snapshotDownloadSource(source)
		if err != nil {
			a.reply(transferCtx, msg, "send snapshot error: "+err.Error())
			return
		}
		defer os.Remove(snapshot)
		if _, err := a.Telegram.SendDocumentNamed(transferCtx, a.Config.TelegramChatID, snapshot, filename, filename); err != nil {
			a.reply(transferCtx, msg, "send error: "+err.Error())
		}
	}) {
		source.Close()
		a.reply(ctx, msg, "send unavailable: Engram is stopping or the transfer queue is full")
		return actionResult{Outcome: actionStateFailed, Message: "upload transfer unavailable"}
	}
	return actionResult{Outcome: actionOK, Message: "file send queued"}
}

func openDownloadSource(path string) (*os.File, os.FileInfo, error) {
	before, err := validateDownloadPath(path)
	if err != nil {
		return nil, nil, err
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open error: %w", err)
	}
	after, err := source.Stat()
	if err != nil {
		source.Close()
		return nil, nil, fmt.Errorf("stat opened file: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		source.Close()
		return nil, nil, fmt.Errorf("file changed during validation")
	}
	return source, after, nil
}

func (a *App) snapshotDownloadSource(source *os.File) (string, error) {
	if err := os.MkdirAll(a.Config.ArtifactDir(), 0o700); err != nil {
		return "", err
	}
	snapshot, err := os.CreateTemp(a.Config.ArtifactDir(), "engram-download-*.bin")
	if err != nil {
		return "", err
	}
	path := snapshot.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := snapshot.Chmod(0o600); err != nil {
		snapshot.Close()
		return "", err
	}
	written, copyErr := io.Copy(snapshot, io.LimitReader(source, telegramCloudUploadMax+1))
	closeErr := snapshot.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > telegramCloudUploadMax {
		return "", errArtifactTooLarge
	}
	keep = true
	return path, nil
}

func (a *App) logs(ctx context.Context, msg telegram.Message) {
	b, err := tailAuditLog(a.Config.AuditPath(), 200_000)
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

func tailAuditLog(path string, maxBytes int64) ([]byte, error) {
	current, err := tailFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(current)) >= maxBytes {
		return current, nil
	}
	previous, err := tailFile(path+".1", maxBytes-int64(len(current)))
	if err != nil {
		return nil, err
	}
	return append(previous, current...), nil
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

func (a *App) attachmentHardMax() int64 {
	hardMax := telegramCloudDownloadMax
	if configured := a.Config.AttachmentSoftMaxBytes * 4; configured > 0 && configured < hardMax {
		hardMax = configured
	}
	free := diskFree(a.Config.ArtifactDir())
	if free > 0 {
		available := free / 2
		if free > attachmentDiskReserve {
			available = free - attachmentDiskReserve
		}
		if available < uint64(hardMax) {
			hardMax = int64(available)
		}
	}
	if hardMax < 1 {
		return 1
	}
	return hardMax
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (a *App) queueTransfer(fn func(context.Context)) bool {
	ctx := a.workerContext()
	select {
	case <-ctx.Done():
		return false
	default:
	}
	if a.transferQueue != nil {
		select {
		case a.transferQueue <- struct{}{}:
		default:
			return false
		}
	}
	a.transferWG.Add(1)
	go func() {
		defer a.transferWG.Done()
		if a.transferQueue != nil {
			defer func() { <-a.transferQueue }()
		}
		if !acquireSlot(ctx, a.transferSlots) {
			return
		}
		defer releaseSlot(a.transferSlots)
		fn(ctx)
	}()
	return true
}

func diskFree(path string) uint64 {
	_ = os.MkdirAll(path, 0o700)
	var stat syscallStatfs
	if err := statfs(path, &stat); err != nil {
		return 0
	}
	return stat.Bavail * uint64(stat.Bsize)
}
