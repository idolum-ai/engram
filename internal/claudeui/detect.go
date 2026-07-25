// Package claudeui recognizes a narrow, versioned subset of Claude Code's
// terminal presentation. Unrecognized processes, versions, and layouts pass
// through without semantic removal.
package claudeui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const SupportedVersion = "2.1.219"
const supportedFixtureVersion = "2.1.206"
const maxProcessOutputBytes = 2 << 20

type Runtime struct {
	Detected  bool
	Version   string
	Supported bool
	Identity  string
}

type CommandRunner interface {
	Run(context.Context, string, ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

type boundedOutput struct {
	data []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	remaining := maxProcessOutputBytes - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	return len(p), nil
}

func (b *boundedOutput) String() string { return string(b.data) }

type Detector struct {
	Runner      CommandRunner
	Executables ExecutableResolver
	Versions    VersionResolver
}

func NewDetector() *Detector {
	return &Detector{Runner: ExecRunner{}, Executables: OSExecutableResolver{}, Versions: PathVersionResolver{}}
}

func (d *Detector) Detect(ctx context.Context, panePID int, foreground string) (Runtime, error) {
	if d == nil || d.Runner == nil || panePID <= 0 || !possibleClaudeForeground(foreground) {
		return Runtime{}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := d.Runner.Run(probeCtx, "ps", "-axo", "pid=,ppid=,comm=,args=")
	if err != nil {
		return Runtime{}, err
	}
	candidates := nearestDescendantClaudeProcesses(parseProcesses(out), panePID)
	if len(candidates) == 0 {
		return Runtime{}, nil
	}
	if len(candidates) != 1 {
		return Runtime{Detected: true}, nil
	}
	if d.Versions == nil {
		return Runtime{Detected: true}, fmt.Errorf("Claude Code version resolver is unavailable")
	}
	candidate := candidates[0]
	executable := candidate.path
	if executable == "" {
		if d.Executables == nil {
			return Runtime{Detected: true}, fmt.Errorf("Claude Code executable resolver is unavailable")
		}
		executable, err = d.Executables.Resolve(candidate.pid)
		if err != nil {
			return Runtime{Detected: true}, fmt.Errorf("resolve running Claude Code executable: %w", err)
		}
	}
	if !filepath.IsAbs(executable) || !isClaudeExecutablePath(executable) {
		return Runtime{Detected: true}, fmt.Errorf("running Claude Code executable is not a recognized absolute path")
	}
	version, err := d.Versions.Resolve(executable)
	if err != nil {
		return Runtime{Detected: true}, err
	}
	started, err := d.Runner.Run(probeCtx, "ps", "-o", "lstart=", "-p", strconv.Itoa(candidate.pid))
	if err != nil || strings.TrimSpace(started) == "" {
		if err == nil {
			err = fmt.Errorf("empty process start time")
		}
		return Runtime{Detected: true, Version: version}, fmt.Errorf("identify Claude Code process incarnation: %w", err)
	}
	return Runtime{
		Detected: true, Version: version, Supported: supportedVersion(version),
		Identity: runtimeIdentity(candidate.pid, executable, version, strings.TrimSpace(started)),
	}, nil
}

func supportedVersion(version string) bool {
	return version == SupportedVersion || version == supportedFixtureVersion
}

func possibleClaudeForeground(command string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(command))) {
	case "claude", "node", "nodejs":
		return true
	default:
		return false
	}
}

type process struct {
	pid  int
	ppid int
	comm string
	args string
}

func parseProcesses(out string) []process {
	var processes []process
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil || pid <= 0 || ppid < 0 {
			continue
		}
		processes = append(processes, process{pid: pid, ppid: ppid, comm: fields[2], args: strings.Join(fields[3:], " ")})
	}
	return processes
}

type executableCandidate struct {
	pid   int
	path  string
	depth int
}

func nearestDescendantClaudeProcesses(processes []process, root int) []executableCandidate {
	depths := map[int]int{root: 0}
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			parentDepth, parentKnown := depths[process.ppid]
			depth, known := depths[process.pid]
			if parentKnown && (!known || parentDepth+1 < depth) {
				depths[process.pid] = parentDepth + 1
				changed = true
			}
		}
	}
	var candidates []executableCandidate
	seen := make(map[string]bool)
	for _, process := range processes {
		depth, descendant := depths[process.pid]
		if !descendant || !possibleClaudeProcess(process) {
			continue
		}
		path := claudeExecutable(process)
		key := strconv.Itoa(process.pid)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, executableCandidate{pid: process.pid, path: path, depth: depth})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		if candidates[i].pid != candidates[j].pid {
			return candidates[i].pid < candidates[j].pid
		}
		return candidates[i].path < candidates[j].path
	})
	if len(candidates) == 0 {
		return nil
	}
	nearestDepth := candidates[0].depth
	end := 0
	for end < len(candidates) && candidates[end].depth == nearestDepth {
		end++
	}
	return candidates[:end]
}

func possibleClaudeProcess(process process) bool {
	if claudeExecutable(process) != "" {
		return true
	}
	if strings.EqualFold(filepath.Base(strings.TrimSpace(process.comm)), "claude") {
		return true
	}
	fields := strings.Fields(process.args)
	return len(fields) > 0 && strings.EqualFold(filepath.Base(strings.Trim(fields[0], "'\"")), "claude")
}

func claudeExecutable(process process) string {
	for _, field := range strings.Fields(process.args) {
		candidate := strings.Trim(field, "'\"")
		if filepath.IsAbs(candidate) && isClaudeExecutablePath(candidate) {
			return candidate
		}
	}
	if filepath.IsAbs(process.comm) && isClaudeExecutablePath(process.comm) {
		return process.comm
	}
	return ""
}

type ExecutableResolver interface {
	Resolve(int) (string, error)
}

type OSExecutableResolver struct{}

func (OSExecutableResolver) Resolve(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process ID")
	}
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("running executable resolution is unsupported on %s", runtime.GOOS)
	}
	path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", err
	}
	return path, nil
}

func isClaudeExecutablePath(path string) bool {
	if filepath.Base(path) == "claude" {
		return true
	}
	dir := filepath.Dir(path)
	return filepath.Base(dir) == "versions" && filepath.Base(filepath.Dir(dir)) == "claude" && validVersion(filepath.Base(path))
}

type VersionResolver interface {
	Resolve(string) (string, error)
}

type PathVersionResolver struct{}

func (PathVersionResolver) Resolve(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("Claude Code executable path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Claude Code executable: %w", err)
	}
	if !isClaudeExecutablePath(resolved) {
		return "", fmt.Errorf("resolved executable is not in Claude Code's versioned installation")
	}
	version := filepath.Base(resolved)
	if !validVersion(version) {
		return "", fmt.Errorf("Claude Code version is not encoded in its executable path")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat Claude Code executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Claude Code executable is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("Claude Code executable is not executable")
	}
	return version, nil
}

func validVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func runtimeIdentity(pid int, path, version, started string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(pid) + "\x00" + path + "\x00" + version + "\x00" + started))
	return hex.EncodeToString(sum[:])
}
