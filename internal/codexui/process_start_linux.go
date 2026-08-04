//go:build linux

package codexui

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func kernelProcessStart(pid int) (time.Time, string, error) {
	if pid <= 0 {
		return time.Time{}, "", fmt.Errorf("invalid process id")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read process stat: %w", err)
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return time.Time{}, "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) <= 19 {
		return time.Time{}, "", fmt.Errorf("invalid process stat")
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return time.Time{}, "", fmt.Errorf("invalid process start ticks")
	}
	clockRaw, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read process clock rate: %w", err)
	}
	clockTicks, err := strconv.ParseUint(strings.TrimSpace(string(clockRaw)), 10, 64)
	if err != nil || clockTicks == 0 {
		return time.Time{}, "", fmt.Errorf("invalid process clock rate")
	}
	procStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read boot time: %w", err)
	}
	var bootSeconds int64
	for _, line := range strings.Split(string(procStat), "\n") {
		if strings.HasPrefix(line, "btime ") {
			bootSeconds, err = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			break
		}
	}
	if err != nil || bootSeconds <= 0 {
		return time.Time{}, "", fmt.Errorf("invalid boot time")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(bootID)) == "" {
		return time.Time{}, "", fmt.Errorf("read boot identity")
	}
	uptimeSeconds := startTicks / clockTicks
	uptimeNanos := startTicks % clockTicks * uint64(time.Second) / clockTicks
	started := time.Unix(bootSeconds+int64(uptimeSeconds), int64(uptimeNanos)).UTC()
	return started, fmt.Sprintf("linux:%s:%d", strings.TrimSpace(string(bootID)), startTicks), nil
}
