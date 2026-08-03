// Package codexui recognizes a narrow, versioned subset of Codex's terminal
// presentation. Unrecognized processes, versions, and layouts pass through.
package codexui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
const supportedPreviousVersion = "0.144.5"
const maxProcessOutputBytes = 2 << 20

type Runtime struct {
	Detected  bool
	Version   string
	Supported bool
	// Identity changes when the running Codex process is replaced, even when
	// its PID or executable path is reused.
	Identity  string
	StartedAt time.Time
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
	executables := nearestDescendantCodexExecutables(parseProcesses(out), panePID)
	if len(executables) == 0 {
		return Runtime{}, nil
	}
	if len(executables) != 1 {
		return Runtime{Detected: true}, nil
	}
	if d.Versions == nil {
		return Runtime{Detected: true}, fmt.Errorf("Codex version resolver is unavailable")
	}
	candidate := executables[0]
	version, err := d.Versions.Resolve(candidate.path)
	if err != nil {
		return Runtime{Detected: true}, err
	}
	startedRaw, err := d.Runner.Run(probeCtx, "ps", "-o", "lstart=", "-p", strconv.Itoa(candidate.pid))
	if err != nil || strings.TrimSpace(startedRaw) == "" {
		// Process-incarnation proof is required for transcript context, not for
		// the existing versioned visible-screen adapter. Preserve that literal
		// compatibility boundary when this optional identity probe is unavailable.
		return Runtime{Detected: true, Version: version, Supported: supportedVersion(version)}, nil
	}
	started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(startedRaw), time.Local)
	if err != nil {
		return Runtime{Detected: true, Version: version, Supported: supportedVersion(version)}, nil
	}
	runtime := Runtime{
		Detected: true, Version: version, Supported: supportedVersion(version), StartedAt: started,
		Identity: runtimeIdentity(candidate.pid, candidate.path, version, strings.TrimSpace(startedRaw)),
	}
	return runtime, nil
}

func supportedVersion(version string) bool {
	return version == SupportedVersion || version == supportedPreviousVersion
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

type executableCandidate struct {
	pid   int
	path  string
	depth int
}

func nearestDescendantCodexExecutables(processes []process, root int) []executableCandidate {
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
	byPID := make(map[int]executableCandidate)
	for _, process := range processes {
		depth, descendant := depths[process.pid]
		if !descendant {
			continue
		}
		executable := codexExecutable(process)
		if executable == "" {
			continue
		}
		if previous, exists := byPID[process.pid]; !exists || depth < previous.depth {
			byPID[process.pid] = executableCandidate{pid: process.pid, path: executable, depth: depth}
		}
	}
	candidates := make([]executableCandidate, 0, len(byPID))
	for _, candidate := range byPID {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].path < candidates[j].path
	})
	nearest := len(candidates)
	if nearest > 0 {
		minimumDepth := candidates[0].depth
		nearest = 0
		for nearest < len(candidates) && candidates[nearest].depth == minimumDepth {
			nearest++
		}
	}
	return candidates[:nearest]
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

func runtimeIdentity(pid int, path, version, started string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(pid) + "\x00" + path + "\x00" + version + "\x00" + started))
	return hex.EncodeToString(sum[:])
}
