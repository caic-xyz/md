// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests fractional container inspection and metadata normalization.

package containers

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseInspectInfo(t *testing.T) {
	t.Parallel()
	t.Run("valid_docker", func(t *testing.T) {
		t.Parallel()
		cacheLabel := "opaque-label"
		rw := `[
			{
				"Id":"sha256:container",
				"Name":"/md-test",
				"Image":"sha256:image",
				"Architecture":"amd64",
				"Os":"linux",
				"Config":{"Image":"ghcr.io/caic/base:latest","Labels":{"md.cache_spec":"` + cacheLabel + `"}},
				"State":{"Status":"running"},
				"HostConfig":{"NanoCpus":2500000000,"CpuQuota":0,"CpuPeriod":0,"CpusetCpus":"0-3"},
				"Mounts":[{"Source":"/host/rw","Destination":"/mnt/rw","RW":true},{"Source":"/host/ro","Destination":"/mnt/ro","RW":false}]
			}
		]`
		got, err := parseInspectInfo("docker", "md-test", []byte(rw))
		if err != nil {
			t.Fatal(err)
		}
		if got.Runtime != "docker" || got.ID != "sha256:container" || got.Name != "md-test" || got.ImageID != "sha256:image" || got.ImageRef != "ghcr.io/caic/base:latest" {
			t.Fatalf("inspect identity = %+v", got)
		}
		if got.Platform != "linux/amd64" {
			t.Errorf("Platform = %q, want linux/amd64", got.Platform)
		}
		if got.OS != "linux" || got.Architecture != "amd64" {
			t.Errorf("OS/Architecture = %q/%q, want linux/amd64", got.OS, got.Architecture)
		}
		if !slices.Equal(got.CPUSet, []int{0, 1, 2, 3}) {
			t.Fatalf("CPU constraints = %+v", got)
		}
		if got.CPULimit != 2.5 {
			t.Errorf("CPULimit = %g, want 2.5", got.CPULimit)
		}
		if len(got.Mounts) != 2 || got.Mounts[0].ReadOnly || !got.Mounts[1].ReadOnly {
			t.Errorf("Mounts = %+v", got.Mounts)
		}
		if got.Labels["md.cache_spec"] != cacheLabel {
			t.Fatalf("Labels = %v", got.Labels)
		}
	})

	t.Run("valid_podman", func(t *testing.T) {
		t.Parallel()
		rw := `[
			{
				"Id":"ctr",
				"Image":"sha256:image2",
				"Platform":"linux/arm64",
				"Config":{"Image":"base:v2","Labels":{}},
				"State":{"Status":"exited"},
				"HostConfig":{"NanoCpus":0,"CpuQuota":150000,"CpuPeriod":100000},
				"Mounts":[{"Source":"/src","Destination":"/workspace"}]
			}
		]`
		got, err := parseInspectInfo("podman", "ctr-2", []byte(rw))
		if err != nil {
			t.Fatal(err)
		}
		if got.Platform != "linux/arm64" {
			t.Errorf("Platform = %q, want linux/arm64", got.Platform)
		}
		if got.OS != "linux" || got.Architecture != "arm64" {
			t.Errorf("OS/Architecture = %q/%q, want linux/arm64", got.OS, got.Architecture)
		}
		if got.CPULimit != 1.5 {
			t.Errorf("CPULimit = %g, want 1.5", got.CPULimit)
		}
		if got.Name != "ctr-2" {
			t.Errorf("Name = %q, want ctr-2", got.Name)
		}
	})

	t.Run("valid_default_period", func(t *testing.T) {
		t.Parallel()
		got, err := parseInspectInfo("docker", "limited", []byte(`[{"HostConfig":{"CpuQuota":150000}}]`))
		if err != nil {
			t.Fatal(err)
		}
		if got.CPULimit != 1.5 {
			t.Fatalf("CPULimit = %g, want 1.5", got.CPULimit)
		}
	})
	t.Run("valid_unrestricted", func(t *testing.T) {
		t.Parallel()
		got, err := parseInspectInfo("docker", "unrestricted", []byte(`[{"HostConfig":{"CpuQuota":-1,"CpuPeriod":100000}}]`))
		if err != nil {
			t.Fatal(err)
		}
		if got.CPULimit != 0 {
			t.Fatalf("CPULimit = %g, want unrestricted", got.CPULimit)
		}
	})

	t.Run("valid_cpuset", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name, set string
			want      []int
		}{
			{"inherited", "", nil},
			{"unsorted", "7,1,4", []int{1, 4, 7}},
			{"overlapping", "3-6,1-4", []int{1, 2, 3, 4, 5, 6}},
			{"duplicate", "2,2,1-2,2", []int{1, 2}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				raw, err := json.Marshal([]any{map[string]any{"HostConfig": map[string]string{"CpusetCpus": tt.set}}})
				if err != nil {
					t.Fatal(err)
				}
				got, err := parseInspectInfo("docker", "test", raw)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got.CPUSet, tt.want) {
					t.Fatalf("CPUSet = %v, want %v", got.CPUSet, tt.want)
				}
			})
		}
	})
	t.Run("error_cpuset", func(t *testing.T) {
		t.Parallel()
		for _, set := range []string{"-1", "3-1", "a", "0-", "1,,2", "1-2-3", "1000001"} {
			t.Run(set, func(t *testing.T) {
				t.Parallel()
				raw, err := json.Marshal([]any{map[string]any{"HostConfig": map[string]string{"CpusetCpus": set}}})
				if err != nil {
					t.Fatal(err)
				}
				got, err := parseInspectInfo("docker", "test", raw)
				if err == nil || !strings.Contains(err.Error(), "invalid CPU set") || got != nil {
					t.Fatalf("inspect = %+v, %v", got, err)
				}
			})
		}
	})

	t.Run("error_empty", func(t *testing.T) {
		t.Parallel()
		if _, err := parseInspectInfo("docker", "ctr", []byte(`[]`)); err == nil {
			t.Fatal("parseInspectInfo error = nil, want error")
		}
	})
}
