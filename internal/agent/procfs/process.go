package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessInfo struct {
	PID        int
	PPID       int
	StartTicks uint64
	Comm       string
	CGroup     string
	NetNS      string
}

func Scan(root string) ([]ProcessInfo, int, error) {
	if root == "" {
		root = "/proc"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, 0, fmt.Errorf("read proc root: %w", err)
	}
	processes := make([]ProcessInfo, 0, len(entries))
	failed := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		process, err := Read(root, pid)
		if err != nil {
			failed++
			continue
		}
		processes = append(processes, process)
	}
	return processes, failed, nil
}

func Read(root string, pid int) (ProcessInfo, error) {
	if root == "" {
		root = "/proc"
	}
	if pid <= 0 {
		return ProcessInfo{}, fmt.Errorf("invalid pid %d", pid)
	}
	base := filepath.Join(root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return ProcessInfo{}, fmt.Errorf("read process stat: %w", err)
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	openParen := strings.IndexByte(string(stat), '(')
	if openParen < 0 || closeParen <= openParen {
		return ProcessInfo{}, fmt.Errorf("malformed process stat for pid %d", pid)
	}
	comm := string(stat[openParen+1 : closeParen])
	fields := strings.Fields(string(stat[closeParen+1:]))
	// fields[0] is stat field 3 (state); starttime is field 22.
	if len(fields) <= 19 {
		return ProcessInfo{}, fmt.Errorf("short process stat for pid %d", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return ProcessInfo{}, fmt.Errorf("invalid ppid for pid %d", pid)
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return ProcessInfo{}, fmt.Errorf("invalid starttime for pid %d", pid)
	}
	cgroupData, cgroupErr := os.ReadFile(filepath.Join(base, "cgroup"))
	if cgroupErr != nil {
		return ProcessInfo{}, fmt.Errorf("read process cgroup: %w", cgroupErr)
	}
	netNS := ""
	if target, err := os.Readlink(filepath.Join(base, "ns", "net")); err == nil {
		if left := strings.IndexByte(target, '['); left >= 0 {
			if right := strings.IndexByte(target[left+1:], ']'); right >= 0 {
				netNS = target[left+1 : left+1+right]
			}
		}
	}
	return ProcessInfo{
		PID:        pid,
		PPID:       ppid,
		StartTicks: startTicks,
		Comm:       comm,
		CGroup:     strings.TrimSpace(string(cgroupData)),
		NetNS:      netNS,
	}, nil
}
