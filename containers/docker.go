// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Docker CLI runtime implementation.

package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
)

// newDocker returns a Docker runtime wrapper.
func newDocker(executable string, logger *slog.Logger, env []string) *docker {
	if executable == "" {
		executable = "docker"
	}
	return &docker{base: newBase(executable, logger, env, parseDockerStats)}
}

// docker wraps the docker CLI.
type docker struct {
	base
}

// Info queries the Docker server's CPU capacity and isolation environment.
func (d *docker) Info(ctx context.Context) (Info, error) {
	out, err := d.Run(ctx, "", "info", "--format", "{{json .}}")
	if err != nil {
		return Info{}, fmt.Errorf("querying runtime server info: %w", err)
	}
	var raw dockerInfoJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Info{}, fmt.Errorf("parsing runtime server info: %w", err)
	}
	if raw.NCPU < 1 {
		return Info{}, fmt.Errorf("invalid runtime CPU count: %d", raw.NCPU)
	}
	return Info{
		CPUs: raw.NCPU, DockerDesktop: raw.OperatingSystem == "Docker Desktop",
		Rootless: slices.Contains(raw.SecurityOptions, "name=rootless"),
	}, nil
}

// UntagImage removes an image tag without deleting containers that use it.
func (d *docker) UntagImage(ctx context.Context, image string) error {
	_, err := d.Run(ctx, "", "rmi", "-f", "--no-prune", image)
	return err
}

type dockerInfoJSON struct {
	NCPU            int      `json:"NCPU"`
	OperatingSystem string   `json:"OperatingSystem"`
	SecurityOptions []string `json:"SecurityOptions"`
}

type dockerStats struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	PIDs     string `json:"PIDs"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
}

// parseDockerStats parses one JSON line from Docker stats output.
func parseDockerStats(line string) (*Stats, string, error) {
	var raw dockerStats
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, "", fmt.Errorf("parsing Docker stats JSON: %w", err)
	}
	cpuPerc, err := parsePercent(raw.CPUPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing CPU%%: %w", err)
	}
	memPerc, err := parsePercent(raw.MemPerc)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem%%: %w", err)
	}
	memUsed, memLimit, err := parseMemUsage(raw.MemUsage)
	if err != nil {
		return nil, "", fmt.Errorf("parsing mem usage: %w", err)
	}
	var pids int
	if raw.PIDs != "N/A" {
		pids, err = strconv.Atoi(raw.PIDs)
		if err != nil {
			return nil, "", fmt.Errorf("parsing PIDs: %w", err)
		}
	}
	netRx, netTx, err := parseIOPair(raw.NetIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing net I/O: %w", err)
	}
	blockRead, blockWrite, err := parseIOPair(raw.BlockIO)
	if err != nil {
		return nil, "", fmt.Errorf("parsing block I/O: %w", err)
	}
	return &Stats{CPUPerc: cpuPerc, MemUsed: memUsed, MemLimit: memLimit, MemPerc: memPerc, PIDs: pids, NetRx: netRx, NetTx: netTx, BlockRead: blockRead, BlockWrite: blockWrite, DiskUsed: -1}, raw.Name, nil
}
