// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
)

// podman wraps the podman CLI.
type podman struct {
	base
}

// newPodman returns a Podman runtime wrapper.
func newPodman(executable string, logger *slog.Logger, env []string) *podman {
	if executable == "" {
		executable = "podman"
	}
	return &podman{base: newBase(executable, logger, env, parsePodmanStats)}
}

// UntagImage removes an image tag without deleting containers that use it.
func (p *podman) UntagImage(ctx context.Context, image string) error {
	_, err := p.Run(ctx, "", "image", "untag", image)
	return err
}

// IsRootless reports whether Podman is running rootless.
//
// In rootless Podman on Linux, the default user namespace maps host UID 1000 to
// container UID 0, so bind-mounted host directories appear root-owned inside
// the container. Callers use this to inject --userns=keep-id so the host UID
// maps to the same UID inside the container.
//
// Caveat: keep-id ownership does not survive `podman commit`, so callers that
// snapshot containers (fork) must repair ownership. See docs/ROOTLESS.md.
func (p *podman) IsRootless() bool {
	return isRootlessPodman()
}

func isRootlessPodman() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return os.Getuid() != 0
}

type podmanStats struct {
	Name        string  `json:"Name"`
	CPU         float64 `json:"CPU"`
	MemUsage    uint64  `json:"MemUsage"`
	MemLimit    uint64  `json:"MemLimit"`
	MemPerc     float64 `json:"MemPerc"`
	PIDs        uint64  `json:"PIDs"`
	NetInput    uint64  `json:"NetInput"`
	NetOutput   uint64  `json:"NetOutput"`
	BlockInput  uint64  `json:"BlockInput"`
	BlockOutput uint64  `json:"BlockOutput"`
}

func parsePodmanStats(line string) (*Stats, string, error) {
	var raw podmanStats
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, "", fmt.Errorf("parsing Podman stats JSON: %w", err)
	}
	pids, err := strconv.Atoi(strconv.FormatUint(raw.PIDs, 10))
	if err != nil {
		return nil, "", fmt.Errorf("parsing PIDs: %w", err)
	}
	return &Stats{CPUPerc: raw.CPU, MemUsed: raw.MemUsage, MemLimit: raw.MemLimit, MemPerc: raw.MemPerc, PIDs: pids, NetRx: raw.NetInput, NetTx: raw.NetOutput, BlockRead: raw.BlockInput, BlockWrite: raw.BlockOutput, DiskUsed: -1}, raw.Name, nil
}
