// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Docker operations and image building.

package md

import (
	"embed"
	"fmt"
	"runtime"
	"strings"
)

// DefaultMaxCPUs returns max(2, NumCPU-2), a sensible CPU limit that
// leaves headroom for the host while guaranteeing at least 2 cores.
func DefaultMaxCPUs() int {
	return max(2, runtime.NumCPU()-2)
}

// Platform is a Linux container platform.
type Platform string

const (
	// PlatformDefault uses the host's native Linux container platform.
	PlatformDefault Platform = ""
	// PlatformLinuxARM64 is the Linux arm64 container platform.
	PlatformLinuxARM64 Platform = "linux/arm64"
	// PlatformLinuxAMD64 is the Linux amd64 container platform.
	PlatformLinuxAMD64 Platform = "linux/amd64"
)

// DefaultPlatform returns the host's native Linux container platform.
func DefaultPlatform() Platform {
	return Platform("linux/" + runtime.GOARCH)
}

// Resolve returns the host's native Linux container platform when p is empty.
func (p Platform) Resolve() Platform {
	if p == PlatformDefault {
		return DefaultPlatform()
	}
	return p
}

// String returns p as a Docker platform string.
func (p Platform) String() string {
	return string(p)
}

// Validate returns an error unless p is a supported Linux container platform or
// PlatformDefault.
func (p Platform) Validate() error {
	switch p {
	case PlatformDefault, PlatformLinuxAMD64, PlatformLinuxARM64:
		return nil
	default:
		return fmt.Errorf("unsupported platform %q; use linux/amd64 or linux/arm64", p)
	}
}

// Architecture returns the platform architecture component.
func (p Platform) Architecture() (string, error) {
	p = p.Resolve()
	if err := p.Validate(); err != nil {
		return "", err
	}
	return strings.TrimPrefix(p.String(), "linux/"), nil
}

//go:embed all:rsc
var rscFS embed.FS

// FormatBytes formats n bytes as a human-readable string (e.g. "1.2 GB").
func FormatBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
