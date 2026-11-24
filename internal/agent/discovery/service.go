package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strconv"
	"strings"

	"compute-audit/internal/agent/procfs"
	"compute-audit/internal/contract"
)

var (
	dockerPathID  = regexp.MustCompile(`(?:^|/)docker/([0-9a-fA-F]{64})(?:$|/)`)
	dockerScopeID = regexp.MustCompile(`(?:docker|cri-containerd|crio|libpod)-([0-9a-fA-F]{64})\.scope`)
)

func Resolve(hostID string, process procfs.ProcessInfo) contract.Service {
	for _, id := range [][]string{dockerPathID.FindStringSubmatch(process.CGroup), dockerScopeID.FindStringSubmatch(process.CGroup)} {
		if len(id) == 2 {
			containerID := strings.ToLower(id[1])
			return contract.Service{
				ID:   serviceID("docker", hostID, containerID),
				Type: "docker",
				Name: containerID,
			}
		}
	}

	var unit string
	for _, component := range strings.Split(process.CGroup, "/") {
		component = path.Base(component)
		if strings.HasSuffix(component, ".service") {
			unit = component
		}
	}
	if unit != "" {
		return contract.Service{
			ID:   serviceID("systemd", hostID, unit),
			Type: "systemd",
			Name: unit,
		}
	}
	return contract.Service{
		ID:   "process:" + hostID + ":" + strconv.Itoa(process.PID) + ":" + strconv.FormatUint(process.StartTicks, 10),
		Type: "process",
		Name: process.Comm,
	}
}

func serviceID(kind, hostID, identity string) string {
	value := kind + ":" + hostID + ":" + identity
	if len(value) <= 191 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return kind + ":" + hex.EncodeToString(digest[:])
}
