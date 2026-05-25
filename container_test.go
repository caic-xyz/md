// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "''"},
		{"simple", "simple", "simple"},
		{"spaces", "hello world", "'hello world'"},
		{"single_quote", "it's", `'it'\''s'`},
		{"multiple_quotes", "a'b'c", `'a'\''b'\''c'`},
		{"safe_path", "safe-path/to_file.txt", "safe-path/to_file.txt"},
		{"with_spaces", "with spaces", "'with spaces'"},
		{"semicolon", "with;semi", "'with;semi'"},
		{"dollar_cmd", "$(cmd)", "'$(cmd)'"},
		{"backslash", `back\slash`, `'back\slash'`},
		{"newline", "hello\nworld", "'hello\nworld'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnmarshalContainer(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if ct.Name != "md-repo-main" {
			t.Errorf("ContainerName = %q, want %q", ct.Name, "md-repo-main")
		}
		if ct.State != "running" {
			t.Errorf("State = %q, want %q", ct.State, "running")
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
		if time.Since(ct.CreatedAt) <= 0 {
			t.Error("CreatedAt is in the future")
		}
	})
	t.Run("with_labels", func(t *testing.T) {
		reposData, _ := json.Marshal([]Repo{{GitRoot: "/home/user/repo", Branch: "main"}})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `,other=ignored"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if ct.Repos[0].GitRoot != "/home/user/repo" {
			t.Errorf("Repos[0].GitRoot = %q, want %q", ct.Repos[0].GitRoot, "/home/user/repo")
		}
		if ct.Repos[0].Branch != "main" {
			t.Errorf("Repos[0].Branch = %q, want %q", ct.Repos[0].Branch, "main")
		}
	})
	t.Run("no_labels", func(t *testing.T) {
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":""}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if ct.Repos != nil {
			t.Errorf("expected nil Repos, got %v", ct.Repos)
		}
	})
	t.Run("empty_repos", func(t *testing.T) {
		// No-repo containers encode md.repos as an empty JSON array.
		reposData, _ := json.Marshal([]Repo{})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-agent","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 0 {
			t.Errorf("expected empty Repos, got %v", ct.Repos)
		}
	})
	t.Run("podman_rfc3339", func(t *testing.T) {
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00.123456789Z"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 123456789, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_no_frac", func(t *testing.T) {
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00Z"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_offset", func(t *testing.T) {
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00+02:00"}`
		ct, err := unmarshalContainer([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.FixedZone("", 2*60*60))
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("bad_created_at", func(t *testing.T) {
		raw := `{"Names":"x","State":"running","CreatedAt":"not-a-date"}`
		_, err := unmarshalContainer([]byte(raw))
		if err == nil {
			t.Fatal("expected error for bad CreatedAt")
		}
	})
	t.Run("empty_input", func(t *testing.T) {
		_, err := unmarshalContainer([]byte(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("bad_json", func(t *testing.T) {
		_, err := unmarshalContainer([]byte("{not json}"))
		if err == nil {
			t.Fatal("expected error for bad JSON")
		}
	})
}

func TestParseStatsLine(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		line := `{"Name":"md-repo-main","CPUPerc":"1.23%","MemUsage":"150MiB / 7.5GiB","MemPerc":"1.95%","PIDs":"12","NetIO":"1.5kB / 500B","BlockIO":"10MB / 2MB"}`
		s, name, err := parseStatsLine(line)
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
	t.Run("na_values", func(t *testing.T) {
		// docker stats returns N/A when cgroup metrics are unavailable (e.g. DinD).
		line := `{"Name":"md-repo-main","CPUPerc":"N/A","MemUsage":"N/A / N/A","MemPerc":"N/A","PIDs":"N/A","NetIO":"N/A / N/A","BlockIO":"N/A / N/A"}`
		s, name, err := parseStatsLine(line)
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
				_, _, err := parseStatsLine(tt.in)
				if err == nil {
					t.Errorf("parseStatsLine(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseByteSize(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
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
				_, err := parseByteSize(tt.in)
				if err == nil {
					t.Errorf("parseByteSize(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseMemUsage(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
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
		used, limit, err := parseMemUsage("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if used != 0 || limit != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", used, limit)
		}
	})

	t.Run("error", func(t *testing.T) {
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
				_, _, err := parseMemUsage(tt.in)
				if err == nil {
					t.Errorf("parseMemUsage(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestParseIOPair(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
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
		a, b, err := parseIOPair("N/A / N/A")
		if err != nil {
			t.Fatal(err)
		}
		if a != 0 || b != 0 {
			t.Errorf("expected (0, 0), got (%d, %d)", a, b)
		}
	})

	t.Run("error", func(t *testing.T) {
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
				_, _, err := parseIOPair(tt.in)
				if err == nil {
					t.Errorf("parseIOPair(%q) should return error", tt.in)
				}
			})
		}
	})
}

func TestMergePaths(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got := mergePaths(nil)
		// Should return alwaysPaths base.
		if len(got.XDGConfigPaths) < 2 {
			t.Errorf("expected at least 2 XDGConfigPaths from alwaysPaths, got %d", len(got.XDGConfigPaths))
		}
	})

	t.Run("merge", func(t *testing.T) {
		input := []AgentPaths{
			{HomePaths: []string{".foo"}, XDGConfigPaths: []string{"bar"}},
			{HomePaths: []string{".baz"}, LocalSharePaths: []string{"qux"}},
		}
		got := mergePaths(input)
		if !slices.Contains(got.HomePaths, ".foo") || !slices.Contains(got.HomePaths, ".baz") {
			t.Errorf("HomePaths = %v, want .foo and .baz", got.HomePaths)
		}
		if !slices.Contains(got.XDGConfigPaths, "bar") {
			t.Errorf("XDGConfigPaths = %v, want bar", got.XDGConfigPaths)
		}
		if !slices.Contains(got.LocalSharePaths, "qux") {
			t.Errorf("LocalSharePaths = %v, want qux", got.LocalSharePaths)
		}
	})

	t.Run("does_not_mutate_global", func(t *testing.T) {
		before := len(alwaysPaths.XDGConfigPaths)
		_ = mergePaths([]AgentPaths{{XDGConfigPaths: []string{"extra1", "extra2"}}})
		after := len(alwaysPaths.XDGConfigPaths)
		if before != after {
			t.Errorf("alwaysPaths.XDGConfigPaths mutated: was %d, now %d", before, after)
		}
	})
}

func TestFillFromInspect(t *testing.T) {
	// Both Docker and Podman inspect return a JSON array.
	inspect := `[{
  "Name": "/md-caic-caic-3",
  "State": { "Status": "running" },
  "Created": "2025-06-15T10:30:00Z",
  "Config": {
    "Labels": {
      "md.display": "1",
      "md.tailscale": "1",
      "md.usb": "1",
      "md.sudo": "1"
    }
  }
}]`

	ct := &Container{}
	if err := fillFromInspect(ct, []byte(inspect)); err != nil {
		t.Fatalf("fillFromInspect: %v", err)
	}
	if ct.Name != "md-caic-caic-3" {
		t.Errorf("Name = %q, want %q", ct.Name, "md-caic-caic-3")
	}
	if ct.State != "running" {
		t.Errorf("State = %q, want %q", ct.State, "running")
	}
	if !ct.Display || !ct.Tailscale || !ct.USB || !ct.Sudo {
		t.Errorf("expected all flags true, got Display=%v Tailscale=%v USB=%v Sudo=%v", ct.Display, ct.Tailscale, ct.USB, ct.Sudo)
	}

	// Name without leading slash (Docker sometimes omits it).
	noSlash := `[{"Name":"plain","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{}}}]`
	ct2 := &Container{}
	if err := fillFromInspect(ct2, []byte(noSlash)); err != nil {
		t.Fatalf("no-slash name: %v", err)
	}
	if ct2.Name != "plain" {
		t.Errorf("Name = %q, want %q", ct2.Name, "plain")
	}

	// Empty array.
	if err := fillFromInspect(&Container{}, []byte(`[]`)); err == nil {
		t.Error("expected error for empty array")
	}
	// Multiple results.
	if err := fillFromInspect(&Container{}, []byte(`[{},{}]`)); err == nil {
		t.Error("expected error for multiple results")
	}
	// Bad JSON.
	if err := fillFromInspect(&Container{}, []byte(`{bad}`)); err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestParseCreatedAt(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"docker", "2025-06-15 10:30:00 +0000 UTC", false},
		{"docker_with_tz", "2025-06-15 10:30:00 -0700 MST", false},
		{"podman_rfc3339nano", "2025-06-15T10:30:00.123456789Z", false},
		{"podman_rfc3339", "2025-06-15T10:30:00Z", false},
		{"podman_rfc3339_offset", "2025-06-15T10:30:00+02:00", false},
		{"invalid", "not-a-date", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCreatedAt(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCreatedAt(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestRepo(t *testing.T) {
	t.Run("Name", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			tests := []struct {
				name string
				r    Repo
				want string
			}{
				{
					"basename only",
					Repo{GitRoot: "/home/user/src/myrepo"},
					"myrepo",
				},
				{
					"basename with .git suffix",
					Repo{GitRoot: "/home/user/src/myrepo.git"},
					"myrepo",
				},
				{
					"MountedName overrides basename",
					Repo{GitRoot: "/home/user/src/myrepo", MountedName: "custom-name"},
					"custom-name",
				},
				{
					"MountedName preserves slashes",
					Repo{GitRoot: "/home/user/src/projects/foo/website", MountedName: "foo/website"},
					"foo/website",
				},
				{
					"empty MountedName falls back to basename",
					Repo{GitRoot: "/home/user/src/myrepo", MountedName: ""},
					"myrepo",
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					if got := tt.r.Name(); got != tt.want {
						t.Errorf("Name() = %q, want %q", got, tt.want)
					}
				})
			}
		})
	})
}

func TestValidateRepoNames(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		tests := []struct {
			name  string
			repos []Repo
		}{
			{
				"single repo",
				[]Repo{{GitRoot: "/home/user/src/myrepo"}},
			},
			{
				"two repos with different basenames",
				[]Repo{
					{GitRoot: "/home/user/src/foo"},
					{GitRoot: "/home/user/src/bar"},
				},
			},
			{
				"same basename but different MountedName",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website", MountedName: "foo/website"},
					{GitRoot: "/home/user/src/bar/website", MountedName: "bar/website"},
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if err := validateRepoNames(tt.repos); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		// Two repos with the same basename but no MountedName disambiguation.
		// Both foo/website and bar/website resolve to Name() == "website".
		repos := []Repo{
			{GitRoot: "/home/user/src/foo/website"},
			{GitRoot: "/home/user/src/bar/website"},
		}
		err := validateRepoNames(repos)
		if err == nil {
			t.Fatal("expected error for duplicate mount names")
		}
		if !strings.Contains(err.Error(), "both mount as") {
			t.Errorf("error should mention 'both mount as', got: %v", err)
		}
		if !strings.Contains(err.Error(), "MountedName") {
			t.Errorf("error should mention 'MountedName', got: %v", err)
		}
	})
}
