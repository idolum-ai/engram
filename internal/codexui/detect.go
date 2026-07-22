// Package codexui recognizes a narrow, versioned subset of Codex's terminal
// presentation. Unrecognized processes, versions, and layouts pass through.
package codexui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const SupportedVersion = "0.144.6"
const maxProcessOutputBytes = 2 << 20

type Runtime struct {
	Detected  bool
	Version   string
	Supported bool
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
	Runner   CommandRunner
	Versions VersionResolver
}

func NewDetector() *Detector {
	return &Detector{Runner: ExecRunner{}, Versions: PackageVersionResolver{}}
}

func (d *Detector) Detect(ctx context.Context, panePID int, foreground string) (Runtime, error) {
	if d == nil || d.Runner == nil || panePID <= 0 || !possibleCodexForeground(foreground) {
		return Runtime{}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := d.Runner.Run(probeCtx, "ps", "-axo", "pid=,ppid=,comm=,args=")
	if err != nil {
		return Runtime{}, err
	}
	executables := descendantCodexExecutables(parseProcesses(out), panePID)
	if len(executables) == 0 {
		return Runtime{}, nil
	}
	if d.Versions == nil {
		return Runtime{Detected: true}, fmt.Errorf("Codex version resolver is unavailable")
	}
	version, err := d.Versions.Resolve(executables[0])
	if err != nil {
		return Runtime{Detected: true}, err
	}
	runtime := Runtime{Detected: true, Version: version, Supported: version == SupportedVersion}
	return runtime, nil
}

func possibleCodexForeground(command string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(command))) {
	case "codex", "node", "nodejs":
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
	lines := strings.Split(out, "\n")
	processes := make([]process, 0, len(lines))
	for _, line := range lines {
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

func descendantCodexExecutables(processes []process, root int) []string {
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
	type candidate struct {
		path  string
		depth int
	}
	byPath := make(map[string]int)
	for _, process := range processes {
		depth, descendant := depths[process.pid]
		if !descendant {
			continue
		}
		executable := codexExecutable(process)
		if executable == "" {
			continue
		}
		if previous, exists := byPath[executable]; !exists || depth < previous {
			byPath[executable] = depth
		}
	}
	candidates := make([]candidate, 0, len(byPath))
	for path, depth := range byPath {
		candidates = append(candidates, candidate{path: path, depth: depth})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].path < candidates[j].path
	})
	executables := make([]string, len(candidates))
	for index, candidate := range candidates {
		executables[index] = candidate.path
	}
	return executables
}

func codexExecutable(process process) string {
	for _, field := range strings.Fields(process.args) {
		candidate := strings.Trim(field, "'\"")
		if filepath.IsAbs(candidate) && filepath.Base(candidate) == "codex" {
			return candidate
		}
	}
	if filepath.IsAbs(process.comm) && filepath.Base(process.comm) == "codex" {
		return process.comm
	}
	return ""
}

type VersionResolver interface {
	Resolve(string) (string, error)
}

type PackageVersionResolver struct{}

func (PackageVersionResolver) Resolve(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("Codex executable path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Codex executable: %w", err)
	}
	dir := filepath.Dir(resolved)
	for depth := 0; depth < 10; depth++ {
		path := filepath.Join(dir, "package.json")
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().IsRegular() && info.Size() <= 1<<20 {
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", fmt.Errorf("read Codex package metadata: %w", readErr)
			}
			var metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if jsonErr := json.Unmarshal(contents, &metadata); jsonErr == nil && metadata.Name == "@openai/codex" && metadata.Version != "" {
				return metadata.Version, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("@openai/codex package metadata not found")
}
