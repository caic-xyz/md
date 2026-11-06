// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests CLI memory flag defaults, exact size conversions, and runtime percentages.

package main

import (
	"context"
	"errors"
	"flag"
	"testing"

	"github.com/caic-xyz/md/containers"
)

type memoryRuntime struct {
	containers.Runtime

	total uint64
	err   error
	calls int
}

func (r *memoryRuntime) Info(context.Context) (containers.Info, error) {
	r.calls++
	return containers.Info{MemTotal: r.total}, r.err
}

func TestMemoryLimit(t *testing.T) {
	t.Parallel()
	t.Run("resolve", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				value        string
				total, bytes uint64
				queries      int
			}{
				{value: "0"},
				{value: "0%"},
				{value: "0GiB"},
				{value: "08", bytes: 8},
				{value: "67108864", bytes: 64 << 20},
				{value: "18446744073709551615", bytes: ^uint64(0)},
				{value: "4GiB", bytes: 4 << 30},
				{value: "4g", bytes: 4 << 30},
				{value: "4GB", bytes: 4000000000},
				{value: "75%", total: 16 << 30, bytes: 12 << 30, queries: 1},
				{value: "25%", total: 16 << 30, bytes: 4 << 30, queries: 1},
				{value: "75%", total: 16000000001, bytes: 12000000000, queries: 1},
				{value: "100%", total: ^uint64(0), bytes: ^uint64(0), queries: 1},
			} {
				t.Run(tc.value, func(t *testing.T) {
					t.Parallel()
					var m memoryLimit
					err := m.Set(tc.value)
					if err != nil {
						t.Fatal(err)
					}
					r := &memoryRuntime{total: tc.total}
					if tc.queries == 0 {
						r.err = errors.New("RAM metadata unavailable")
					}
					b, err := m.resolve(t.Context(), r)
					if err != nil || b != tc.bytes || r.calls != tc.queries {
						t.Fatalf("resolve = %d, %v (%d queries); want %d, nil (%d queries)", b, err, r.calls, tc.bytes, tc.queries)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				total uint64
				err   error
			}{
				{name: "unknown_RAM"},
				{name: "rounds_to_zero", total: 1},
				{name: "runtime_error", err: errors.New("runtime unavailable")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var m memoryLimit
					err := m.Set("75%")
					if err != nil {
						t.Fatal(err)
					}
					_, err = m.resolve(t.Context(), &memoryRuntime{total: tc.total, err: tc.err})
					if err == nil {
						t.Fatal("expected a percentage resolution error")
					}
					if tc.err != nil && !errors.Is(err, tc.err) {
						t.Fatalf("lost runtime error: %v", err)
					}
				})
			}
		})
	})
	t.Run("Set", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"", " 4GiB", "4GiB ", "4 GiB", "75 %", "75%\t", "-1", "-25%", "101%", "12.5%", "1.5GiB", "NaN", "Inf", "1e3", "4GiBB", "1/2", "4.3.2g", "18446744073709551616", "16EiB", "0.5B"} {
			t.Run(s, func(t *testing.T) {
				t.Parallel()
				var m memoryLimit
				if err := m.Set(s); err == nil {
					t.Fatalf("accepted invalid memory limit %q", s)
				}
			})
		}
	})
	t.Run("flag", func(t *testing.T) {
		t.Parallel()
		fs := flag.NewFlagSet("memory", flag.ContinueOnError)
		var m memoryLimit
		fs.Var(&m, "memory", "Memory limit")
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		r := &memoryRuntime{err: errors.New("RAM metadata unavailable")}
		if b, err := m.resolve(t.Context(), r); err != nil || b != 0 || r.calls != 0 {
			t.Fatalf("default memory = %d, %v (%d RAM queries)", b, err, r.calls)
		}
		if m.String() != "" {
			t.Fatalf("unset memory flag = %q", m.String())
		}
		for _, value := range []string{"75%", "4194304", "4GiB", "25%", "0%", "0"} {
			if err := fs.Set("memory", value); err != nil {
				t.Fatal(err)
			}
			if got := m.String(); got != value {
				t.Fatalf("memory flag = %q; want %q", got, value)
			}
		}
		if err := fs.Set("memory", "invalid"); err == nil || m.String() != "0" {
			t.Fatalf("invalid flag changed memory: %s, %v", &m, err)
		}
	})
}
