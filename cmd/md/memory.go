// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// CLI memory flags resolve byte sizes and percentages against the owning runtime's RAM.

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/caic-xyz/md/containers"
)

// memoryLimit stores the raw and parsed CLI limit; an unset value resolves to unlimited.
type memoryLimit struct {
	raw     string
	bytes   uint64
	percent uint64
}

func (m *memoryLimit) Set(value string) error {
	i := 0
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	n, err := strconv.ParseUint(value[:i], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid memory limit %q: expected an unsigned integer", value)
	}
	unit := strings.ToLower(value[i:])
	if unit == "%" {
		if n > 100 {
			return fmt.Errorf("invalid memory limit %q: percentage must be between 0 and 100", value)
		}
		*m = memoryLimit{raw: value, percent: n}
		return nil
	}
	var multiplier uint64 = 1
	if unit != "" && unit != "b" {
		exponent := strings.IndexByte("kmgtpe", unit[0]) + 1
		if exponent == 0 || (len(unit) > 1 && unit[1:] != "b" && unit[1:] != "ib") {
			return fmt.Errorf("invalid memory limit %q: unknown size unit", value)
		}
		// Explicit SI units (KB, MB, ...) are decimal; IEC units and short
		// suffixes (KiB, K, ...) are binary.
		if len(unit) > 1 && unit[1:] == "b" {
			for range exponent {
				multiplier *= 1000
			}
		} else {
			multiplier = 1 << (10 * exponent)
		}
	}
	if n > ^uint64(0)/multiplier {
		return fmt.Errorf("invalid memory limit %q: exceeds the maximum byte count", value)
	}
	*m = memoryLimit{raw: value, bytes: n * multiplier}
	return nil
}

func (m *memoryLimit) String() string {
	return m.raw
}

func (m *memoryLimit) resolve(ctx context.Context, rt containers.Runtime) (uint64, error) {
	if m.percent == 0 {
		return m.bytes, nil
	}
	info, err := rt.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolving memory percentage: %w", err)
	}
	if info.MemTotal == 0 {
		return 0, errors.New("cannot resolve memory percentage: runtime RAM total is unknown")
	}
	// Divide before multiplying to avoid overflow, preserving whole-byte rounding.
	b := info.MemTotal/100*m.percent + info.MemTotal%100*m.percent/100
	if b == 0 {
		return 0, errors.New("memory limit rounds down to zero bytes")
	}
	return b, nil
}
