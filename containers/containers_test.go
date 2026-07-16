// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			file     string
			wantName string
		}{
			{name: "docker_path", file: "docker", wantName: "docker"},
			{name: "podman_exe_path", file: "podman.exe", wantName: "podman"},
			{name: "unknown_runtime_path", file: "nerdctl", wantName: "nerdctl"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				executable := filepath.Join(t.TempDir(), tt.file)
				r, err := New(executable, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if r.Name() != tt.wantName {
					t.Errorf("Name() = %q, want %q", r.Name(), tt.wantName)
				}
				if r.Executable() != executable {
					t.Errorf("Executable() = %q, want %q", r.Executable(), executable)
				}
			})
		}
	})
}

func TestEnvWithOverrides(t *testing.T) {
	t.Parallel()
	got := EnvWithOverrides(
		[]string{"HOME=/real", "PATH=/bin", "KEEP=1"},
		[]string{"HOME=/tmp/md", "XDG_CONFIG_HOME=/tmp/md/.config"},
	)

	counts := map[string]int{}
	values := map[string]string{}
	for _, kv := range got {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		counts[k]++
		values[k] = v
	}
	if counts["HOME"] != 1 {
		t.Fatalf("HOME count = %d, want 1 in %v", counts["HOME"], got)
	}
	if values["HOME"] != "/tmp/md" {
		t.Fatalf("HOME = %q, want /tmp/md", values["HOME"])
	}
	if values["PATH"] != "/bin" {
		t.Fatalf("PATH = %q, want /bin", values["PATH"])
	}
	if values["XDG_CONFIG_HOME"] != "/tmp/md/.config" {
		t.Fatalf("XDG_CONFIG_HOME = %q, want /tmp/md/.config", values["XDG_CONFIG_HOME"])
	}
}

func TestParseEvent(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
			want Event
		}{
			{
				name: "docker",
				in:   `{"Actor":{"Attributes":{"name":"md-docker","image":"img"}}}`,
				want: Event{Name: "md-docker", Attributes: map[string]string{"image": "img"}},
			},
			{
				name: "podman",
				in:   `{"Name":"md-podman","Attributes":{"image":"img"}}`,
				want: Event{Name: "md-podman", Attributes: map[string]string{"image": "img"}},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ev, ok := parseEvent([]byte(tt.in))
				if !ok {
					t.Fatal("parseEvent returned ok=false")
				}
				if ev.Name != tt.want.Name || ev.Attributes["image"] != tt.want.Attributes["image"] {
					t.Fatalf("event = %+v, want %+v", ev, tt.want)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, ok := parseEvent([]byte(`{"Attributes":{"image":"img"}}`)); ok {
			t.Fatal("parseEvent ok=true, want false")
		}
	})
}

func TestParseImageArchitecture(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			raw  string
			want string
		}{
			{`[{"ImageManifestDescriptor":{"platform":{"architecture":"amd64","os":"linux"}}}]`, "amd64"},
			{`[{"Architecture":"arm64","Os":"linux"}]`, "arm64"},
			{`[{}]`, ""},
			{`[{"ImageManifestDescriptor":{"platform":{"architecture":"amd64","os":"windows"}}}]`, ""},
			{`[{"Architecture":"amd64","Os":"windows"}]`, ""},
		}
		for _, tt := range tests {
			got, err := parseImageArchitecture([]byte(tt.raw))
			if err != nil {
				t.Errorf("parseImageArchitecture(%s): %v", tt.raw, err)
				continue
			}
			if got != tt.want {
				t.Errorf("parseImageArchitecture(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := parseImageArchitecture([]byte(`{bad}`)); err == nil {
			t.Fatal("expected JSON parse error")
		}
	})
}

func TestHasExplicitRegistry(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, image := range []string{"docker.io/library/ubuntu:latest", "ghcr.io/caic-xyz/md-user:latest", "localhost/md-user:latest", "localhost:5000/md-user:latest"} {
			if !hasExplicitRegistry(image) {
				t.Errorf("hasExplicitRegistry(%q) = false, want true", image)
			}
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, image := range []string{"ubuntu:latest", "md-user-local", "myteam/image:latest"} {
			if hasExplicitRegistry(image) {
				t.Errorf("hasExplicitRegistry(%q) = true, want false", image)
			}
		}
	})
}

func TestParseDockerStats(t *testing.T) {
	t.Parallel()
	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		line := `{"Name":"md-repo-main","CPUPerc":"1.23%","MemUsage":"150MiB / 7.5GiB","MemPerc":"1.95%","PIDs":"12","NetIO":"1.5kB / 500B","BlockIO":"10MB / 2MB"}`
		s, name, err := parseDockerStats(line)
		if err != nil {
			t.Fatal(err)
		}
		if name != "md-repo-main" {
			t.Errorf("name = %q, want %q", name, "md-repo-main")
		}
		if s.CPUPerc != 1.23 {
			t.Errorf("CPUPerc = %v, want 1.23", s.CPUPerc)
		}
		if s.MemUsed != 150<<20 {
			t.Errorf("MemUsed = %d, want %d", s.MemUsed, 150<<20)
		}
		if s.PIDs != 12 {
			t.Errorf("PIDs = %d, want 12", s.PIDs)
		}
	})
	t.Run("docker_ansi_frame", func(t *testing.T) {
		t.Parallel()
		line := "\x1b[H{\"Name\":\"md-repo-main\",\"CPUPerc\":\"1.23%\",\"MemUsage\":\"150MiB / 7.5GiB\",\"MemPerc\":\"1.95%\",\"PIDs\":\"12\",\"NetIO\":\"1.5kB / 500B\",\"BlockIO\":\"10MB / 2MB\"}\x1b[K"
		line, err := normalizeStatsLine(line)
		if err != nil {
			t.Fatal(err)
		}
		s, name, err := parseDockerStats(line)
		if err != nil {
			t.Fatal(err)
		}
		if name != "md-repo-main" {
			t.Errorf("name = %q, want %q", name, "md-repo-main")
		}
		if s.CPUPerc != 1.23 {
			t.Errorf("CPUPerc = %v, want 1.23", s.CPUPerc)
		}
	})
	t.Run("na_values", func(t *testing.T) {
		t.Parallel()
		line := `{"Name":"md-repo-main","CPUPerc":"N/A","MemUsage":"N/A / N/A","MemPerc":"N/A","PIDs":"N/A","NetIO":"N/A / N/A","BlockIO":"N/A / N/A"}`
		s, name, err := parseDockerStats(line)
		if err != nil {
			t.Fatalf("N/A values should not cause an error, got: %v", err)
		}
		if name != "md-repo-main" {
			t.Errorf("name = %q, want %q", name, "md-repo-main")
		}
		if s.CPUPerc != 0 || s.MemUsed != 0 || s.MemLimit != 0 || s.NetRx != 0 || s.NetTx != 0 {
			t.Errorf("expected all-zero stats for N/A, got %+v", s)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"empty", ""},
			{"bad_json", "{invalid json}"},
			{"bad_cpu", `{"Name":"x","CPUPerc":"bad%","MemUsage":"0B / 0B","MemPerc":"0%","PIDs":"0","NetIO":"0B / 0B","BlockIO":"0B / 0B"}`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseDockerStats(tt.in)
				if err == nil {
					t.Errorf("parseDockerStats(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParsePodmanStats(t *testing.T) {
	t.Parallel()
	line := `{"AvgCPU":4.902134241779032,"ContainerID":"ec4da855007520bd1702e702cbce0f3346ed1282ec88a273169329e22f2f9be1","Name":"md-caic-caic-28","PerCPU":null,"CPU":4.902134241779032,"CPUNano":6682560000,"CPUSystemNano":1298987,"SystemNano":1784235244400324567,"MemUsage":120074240,"MemLimit":33325363200,"MemPerc":0.3603088712923615,"NetInput":104882,"NetOutput":105535,"BlockInput":0,"BlockOutput":0,"PIDs":27,"UpTime":6682560000,"Duration":6682560000}`
	s, name, err := parsePodmanStats(line)
	if err != nil {
		t.Fatal(err)
	}
	if name != "md-caic-caic-28" {
		t.Errorf("name = %q, want md-caic-caic-28", name)
	}
	if s.CPUPerc != 4.902134241779032 {
		t.Errorf("CPUPerc = %v, want 4.902134241779032", s.CPUPerc)
	}
	if s.MemUsed != 120074240 || s.MemLimit != 33325363200 {
		t.Errorf("memory = %d / %d, want 120074240 / 33325363200", s.MemUsed, s.MemLimit)
	}
	if s.MemPerc != 0.3603088712923615 {
		t.Errorf("MemPerc = %v, want 0.3603088712923615", s.MemPerc)
	}
	if s.PIDs != 27 {
		t.Errorf("PIDs = %d, want 27", s.PIDs)
	}
	if s.NetRx != 104882 || s.NetTx != 105535 {
		t.Errorf("network I/O = %d / %d, want 104882 / 105535", s.NetRx, s.NetTx)
	}
}

func TestNormalizeStatsLine(t *testing.T) {
	t.Parallel()
	line, err := normalizeStatsLine("\x1b[K")
	if err != nil {
		t.Fatal(err)
	}
	if line != "" {
		t.Errorf("normalizeStatsLine() = %q, want empty", line)
	}
}

func TestStripANSICSISequences(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := stripANSICSISequences("\x1b[H{\"Name\":\"container\"}\x1b[K\n\x1b[J")
		if err != nil {
			t.Fatal(err)
		}
		if want := "{\"Name\":\"container\"}\n"; got != want {
			t.Errorf("stripANSICSISequences() = %q, want %q", got, want)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"\x1b", "\x1b[", "\x1b[1"} {
			if _, err := stripANSICSISequences(in); err == nil {
				t.Errorf("stripANSICSISequences(%q) returned nil error", in)
			}
		}
	})
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
			want uint64
		}{
			{"zero_bytes", "0B", 0},
			{"bytes", "100B", 100},
			{"kib", "1KiB", 1 << 10},
			{"mib", "150MiB", 150 << 20},
			{"gib", "7.5GiB", uint64(7.5 * float64(1<<30))},
			{"tib", "1TiB", 1 << 40},
			{"kb", "1.5kB", 1500},
			{"mb", "10MB", 10_000_000},
			{"gb", "1GB", 1_000_000_000},
			{"tb", "2TB", 2_000_000_000_000},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := parseByteSize(tt.in)
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Errorf("parseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"unknown_unit", "100XB"},
			{"no_unit", "100"},
			{"empty", ""},
			{"bad_number", "abcMiB"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, err := parseByteSize(tt.in)
				if err == nil {
					t.Errorf("parseByteSize(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseMemUsage(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		used, limit, err := parseMemUsage("150MiB / 7.5GiB")
		if err != nil {
			t.Fatal(err)
		}
		if used != 150<<20 {
			t.Errorf("used = %d, want %d", used, 150<<20)
		}
		if limit != uint64(7.5*float64(1<<30)) {
			t.Errorf("limit = %d, want %d", limit, uint64(7.5*float64(1<<30)))
		}
	})

	t.Run("na", func(t *testing.T) {
		t.Parallel()
		used, limit, err := parseMemUsage("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if used != 0 || limit != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", used, limit)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"no_slash", "150MiB"},
			{"bad_used", "abc / 1GiB"},
			{"bad_limit", "1MiB / xyz"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseMemUsage(tt.in)
				if err == nil {
					t.Errorf("parseMemUsage(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseIOPair(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		a, b, err := parseIOPair("1.5kB / 500B")
		if err != nil {
			t.Fatal(err)
		}
		if a != 1500 {
			t.Errorf("a = %d, want 1500", a)
		}
		if b != 500 {
			t.Errorf("b = %d, want 500", b)
		}
	})

	t.Run("na", func(t *testing.T) {
		t.Parallel()
		a, b, err := parseIOPair("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if a != 0 || b != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", a, b)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			in   string
		}{
			{"no_slash", "100kB"},
			{"bad_first", "abc / 1kB"},
			{"bad_second", "1kB / xyz"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := parseIOPair(tt.in)
				if err == nil {
					t.Errorf("parseIOPair(%q) should return error", tt.in)
				}
			})
		}
	})
}
