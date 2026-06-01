// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Docker operations and image building.
package md

import (
	"embed"
	"fmt"
	"runtime"
)

// DefaultMaxCPUs returns max(2, NumCPU-2), a sensible CPU limit that
// leaves headroom for the host while guaranteeing at least 2 cores.
func DefaultMaxCPUs() int {
	return max(2, runtime.NumCPU()-2)
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
