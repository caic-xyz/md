// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Podman CLI runtime implementation.

package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// newPodman returns a Podman runtime wrapper.
func newPodman(executable string, logger *slog.Logger, env []string) *podman {
	if executable == "" {
		executable = "podman"
	}
	return &podman{base: newBase(executable, logger, env, parsePodmanStats)}
}

// podman wraps the podman CLI.
type podman struct {
	base
}

// Info queries the Podman server's CPU capacity and isolation environment.
func (p *podman) Info(ctx context.Context) (Info, error) {
	out, err := p.Run(ctx, "", "info", "--format", "{{json .}}")
	if err != nil {
		return Info{}, fmt.Errorf("querying runtime server info: %w", err)
	}
	var raw podmanInfoJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Info{}, fmt.Errorf("parsing runtime server info: %w", err)
	}
	if raw.Host.CPUs < 1 {
		return Info{}, fmt.Errorf("invalid runtime CPU count: %d", raw.Host.CPUs)
	}
	return Info{CPUs: raw.Host.CPUs, Rootless: raw.Host.Security.Rootless}, nil
}

// UntagImage removes an image tag without deleting containers that use it.
func (p *podman) UntagImage(ctx context.Context, image string) error {
	_, err := p.Run(ctx, "", "image", "untag", image)
	return err
}

type podmanInfoJSON struct {
	Host podmanHostInfoJSON `json:"host"`
}

type podmanHostInfoJSON struct {
	CPUs     int                    `json:"cpus"`
	Security podmanSecurityInfoJSON `json:"security"`
}

type podmanSecurityInfoJSON struct {
	Rootless bool `json:"rootless"`
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
