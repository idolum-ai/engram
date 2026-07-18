package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/terminalshot"
)

const snapshotFooterStatusTimeout = 500 * time.Millisecond
const snapshotFooterStatusMaxOutputBytes = 4 << 10

var errSnapshotFooterStatusTooLarge = errors.New("snapshot footer status output is too large")

type snapshotFooterStatusRunner interface {
	Run(context.Context, string, string) (string, error)
}

type shellSnapshotFooterStatusRunner struct{}

func (shellSnapshotFooterStatusRunner) Run(ctx context.Context, command, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = snapshotFooterStatusEnvironment()
	cmd.Stderr = nil
	output := &snapshotFooterStatusBuffer{onOverflow: func() { killSnapshotFooterStatusProcessGroup(cmd) }}
	cmd.Stdout = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killSnapshotFooterStatusProcessGroup(cmd) }
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer killSnapshotFooterStatusProcessGroup(cmd)
	if err := cmd.Wait(); err != nil {
		if output.overflow {
			return "", errSnapshotFooterStatusTooLarge
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if output.overflow {
		return "", errSnapshotFooterStatusTooLarge
	}
	return output.String(), nil
}

func killSnapshotFooterStatusProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

type snapshotFooterStatusBuffer struct {
	value      strings.Builder
	overflow   bool
	onOverflow func()
}

func (b *snapshotFooterStatusBuffer) Write(p []byte) (int, error) {
	remaining := snapshotFooterStatusMaxOutputBytes - b.value.Len()
	if remaining <= 0 {
		b.markOverflow()
		return 0, errSnapshotFooterStatusTooLarge
	}
	if len(p) > remaining {
		_, _ = b.value.Write(p[:remaining])
		b.markOverflow()
		return remaining, errSnapshotFooterStatusTooLarge
	}
	return b.value.Write(p)
}

func (b *snapshotFooterStatusBuffer) String() string { return b.value.String() }

func (b *snapshotFooterStatusBuffer) markOverflow() {
	if b.overflow {
		return
	}
	b.overflow = true
	if b.onOverflow != nil {
		b.onOverflow()
	}
}

func snapshotFooterStatusEnvironment() []string {
	keys := []string{"HOME", "LANG", "LC_ALL", "LC_CTYPE", "LOGNAME", "PATH", "SHELL", "TMPDIR", "USER"}
	environment := make([]string, 0, len(keys))
	hasPath := false
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
			hasPath = hasPath || key == "PATH"
		}
	}
	if !hasPath {
		environment = append(environment, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return environment
}

func (a *App) withSnapshotFooterStatus(ctx context.Context, input terminalshot.Input, cwd string) terminalshot.Input {
	command := strings.TrimSpace(a.Config.SnapshotStatusCommand)
	if command == "" {
		return input
	}
	runner := a.footerStatusRunner
	if runner == nil {
		runner = shellSnapshotFooterStatusRunner{}
	}
	statusCtx, cancel := context.WithTimeout(ctx, snapshotFooterStatusTimeout)
	defer cancel()
	output, err := runner.Run(statusCtx, command, snapshotFooterStatusDir(cwd, a.Config.Workdir))
	if err != nil {
		return input
	}
	status := sanitizeSnapshotFooterStatus(output)
	if status == "" || a.redactText(status) != status || strings.Contains(status, "<redacted") {
		return input
	}
	input.Status = status
	return input
}

func snapshotFooterStatusDir(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		if !filepath.IsAbs(candidate) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return string(filepath.Separator)
}

func sanitizeSnapshotFooterStatus(output string) string {
	output = stripSnapshotFooterControlSequences(output)
	output = strings.ToValidUTF8(output, "")
	var clean strings.Builder
	for _, r := range output {
		switch {
		case unicode.IsSpace(r):
			clean.WriteByte(' ')
		case unicode.IsControl(r), unicode.In(r, unicode.Cf):
			continue
		default:
			clean.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(clean.String()), " ")
}

func stripSnapshotFooterControlSequences(input string) string {
	var output strings.Builder
	for index := 0; index < len(input); {
		if input[index] != 0x1b {
			switch input[index] {
			case 0x9b:
				index = skipSnapshotFooterCSI(input, index+1)
				continue
			case 0x9d:
				index = skipSnapshotFooterControlString(input, index+1, true)
				continue
			case 0x90, 0x98, 0x9e, 0x9f:
				index = skipSnapshotFooterControlString(input, index+1, false)
				continue
			case 0x9c:
				index++
				continue
			}
			r, size := utf8.DecodeRuneInString(input[index:])
			if r == utf8.RuneError && size == 1 {
				index++
				continue
			}
			switch r {
			case '\u009b':
				index = skipSnapshotFooterCSI(input, index+size)
				continue
			case '\u009d':
				index = skipSnapshotFooterControlString(input, index+size, true)
				continue
			case '\u0090', '\u0098', '\u009e', '\u009f':
				index = skipSnapshotFooterControlString(input, index+size, false)
				continue
			}
			output.WriteRune(r)
			index += size
			continue
		}
		index++
		if index >= len(input) {
			break
		}
		switch input[index] {
		case '[':
			index = skipSnapshotFooterCSI(input, index+1)
		case ']':
			index = skipSnapshotFooterControlString(input, index+1, true)
		case 'P', 'X', '^', '_':
			index = skipSnapshotFooterControlString(input, index+1, false)
		default:
			index++
		}
	}
	return output.String()
}

func skipSnapshotFooterCSI(input string, index int) int {
	for index < len(input) && (input[index] < 0x40 || input[index] > 0x7e) {
		index++
	}
	if index < len(input) {
		index++
	}
	return index
}

func skipSnapshotFooterControlString(input string, index int, bellTerminates bool) int {
	for index < len(input) {
		if bellTerminates && input[index] == 0x07 {
			return index + 1
		}
		if input[index] == 0x1b && index+1 < len(input) && input[index+1] == '\\' {
			return index + 2
		}
		if input[index] == 0x9c {
			return index + 1
		}
		r, size := utf8.DecodeRuneInString(input[index:])
		if r == '\u009c' {
			return index + size
		}
		if r == utf8.RuneError && size == 1 {
			index++
			continue
		}
		index += size
	}
	return index
}
