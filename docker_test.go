// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1610612736, "1.5 GB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.in); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{1234, "1,234"},
		{1234567, "1,234,567"},
		{1000000000, "1,000,000,000"},
	}
	for _, tt := range tests {
		if got := formatCount(tt.in); got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveHostPath(t *testing.T) {
	tests := []struct {
		path, home, want string
	}{
		{"~/go/pkg/mod", "/home/alice", "/home/alice/go/pkg/mod"},
		{"~/.cargo/registry", "/home/alice", "/home/alice/.cargo/registry"},
		{"/absolute/path", "/home/alice", "/absolute/path"},
		{"/no/tilde", "/home/alice", "/no/tilde"},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct{ path, home, want string }{`~\go\pkg\mod`, `C:\Users\alice`, `C:/Users/alice/go/pkg/mod`})
	}
	for _, tt := range tests {
		if got := resolveHostPath(tt.path, tt.home); got != tt.want {
			t.Errorf("resolveHostPath(%q, %q) = %q, want %q", tt.path, tt.home, got, tt.want)
		}
	}
}

func TestDirStats(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name, content string
	}{
		{"a.txt", "hello"},
		{"b.txt", "world!"},
		{"sub/c.txt", "foo"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gotFiles, gotBytes := dirStats(dir)
	if gotFiles != 3 {
		t.Errorf("dirStats files = %d, want 3", gotFiles)
	}
	// "hello"=5, "world!"=6, "foo"=3 → 14
	if gotBytes != 14 {
		t.Errorf("dirStats bytes = %d, want 14", gotBytes)
	}
	// Non-existent dir returns zeros.
	f, b := dirStats(filepath.Join(dir, "nonexistent"))
	if f != 0 || b != 0 {
		t.Errorf("dirStats(nonexistent) = (%d, %d), want (0, 0)", f, b)
	}
}

func TestKeysSHA(t *testing.T) {
	keysDir := t.TempDir()
	for _, f := range []struct{ name, content string }{
		{"ssh_host_ed25519_key", "hostkey"},
		{"ssh_host_ed25519_key.pub", "hostkey.pub"},
		{"authorized_keys", "authkeys"},
	} {
		if err := os.WriteFile(filepath.Join(keysDir, f.name), []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("deterministic", func(t *testing.T) {
		got1, err := keysSHA(keysDir)
		if err != nil {
			t.Fatal(err)
		}
		got2, err := keysSHA(keysDir)
		if err != nil {
			t.Fatal(err)
		}
		if got1 != got2 {
			t.Fatalf("expected deterministic hash, got %q then %q", got1, got2)
		}
	})

	t.Run("changes_with_keys", func(t *testing.T) {
		sha1, err := keysSHA(keysDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keysDir, "authorized_keys"), []byte("different"), 0o644); err != nil {
			t.Fatal(err)
		}
		sha2, err := keysSHA(keysDir)
		if err != nil {
			t.Fatal(err)
		}
		if sha1 == sha2 {
			t.Error("keysSHA should change when key content changes")
		}
	})
}

func TestCacheSpecKey(t *testing.T) {
	t.Run("nil_returns_empty", func(t *testing.T) {
		if got := cacheSpecKey(nil); got != "" {
			t.Errorf("cacheSpecKey(nil) = %q, want \"\"", got)
		}
	})
	t.Run("empty_returns_empty", func(t *testing.T) {
		if got := cacheSpecKey([]CacheMount{}); got != "" {
			t.Errorf("cacheSpecKey([]) = %q, want \"\"", got)
		}
	})
	t.Run("non_empty_returns_hex", func(t *testing.T) {
		cm := []CacheMount{{Name: "go-mod", HostPath: "~/go/pkg/mod", ContainerPath: "/home/user/go/pkg/mod"}}
		got := cacheSpecKey(cm)
		if len(got) != 16 {
			t.Errorf("cacheSpecKey len = %d, want 16", len(got))
		}
	})
	t.Run("order_independent", func(t *testing.T) {
		a := []CacheMount{
			{Name: "go-mod", ContainerPath: "/home/user/go/pkg/mod"},
			{Name: "go-build", ContainerPath: "/home/user/.cache/go-build"},
		}
		b := []CacheMount{
			{Name: "go-build", ContainerPath: "/home/user/.cache/go-build"},
			{Name: "go-mod", ContainerPath: "/home/user/go/pkg/mod"},
		}
		if cacheSpecKey(a) != cacheSpecKey(b) {
			t.Error("cacheSpecKey should be order-independent")
		}
	})
	t.Run("different_specs_differ", func(t *testing.T) {
		a := cacheSpecKey([]CacheMount{{Name: "go-mod", ContainerPath: "/home/user/go/pkg/mod"}})
		b := cacheSpecKey([]CacheMount{{Name: "cargo", ContainerPath: "/home/user/.cargo/registry"}})
		if a == b {
			t.Error("different caches should produce different keys")
		}
	})
	t.Run("shallow_differs_from_recursive", func(t *testing.T) {
		a := cacheSpecKey([]CacheMount{{Name: "android-keys", ContainerPath: "/home/user/.android"}})
		b := cacheSpecKey([]CacheMount{{Name: "android-keys", ContainerPath: "/home/user/.android", Shallow: true}})
		if a == b {
			t.Error("shallow and recursive caches with same name/path should produce different keys")
		}
	})
}

func TestResolveCaches(t *testing.T) {
	t.Run("existing_cache_resolved", func(t *testing.T) {
		cacheDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cacheDir, "file.txt"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}

		caches := []CacheMount{{
			Name:          "mycache",
			HostPath:      cacheDir,
			ContainerPath: "/home/user/.cache/myapp",
		}}
		active, dirs, activeKey := resolveCaches(caches, "/home/user", nil)

		if len(active) != 1 || active[0].cm.Name != "mycache" {
			t.Errorf("active = %v, want 1 entry for mycache", active)
		}
		if activeKey == "" {
			t.Error("activeKey should be non-empty when cache dir exists")
		}
		// Should include the cache container path and its intermediary.
		if !slices.Contains(dirs, "/home/user/.cache/myapp") {
			t.Errorf("dirs = %v, want to contain /home/user/.cache/myapp", dirs)
		}
	})

	t.Run("missing_cache_skipped", func(t *testing.T) {
		caches := []CacheMount{{
			Name:          "missing",
			HostPath:      "/nonexistent/path/that/does/not/exist",
			ContainerPath: "/home/user/.cache/missing",
		}}
		active, _, activeKey := resolveCaches(caches, "/home/user", nil)

		if len(active) != 0 {
			t.Errorf("active = %v, want empty", active)
		}
		if activeKey != "" {
			t.Errorf("activeKey = %q, want \"\" for missing cache", activeKey)
		}
	})

	t.Run("mount_paths_included", func(t *testing.T) {
		mountPaths := []string{"/home/user/.amp", "/home/user/.claude"}
		_, dirs, activeKey := resolveCaches(nil, "/home/user", mountPaths)

		if activeKey != "" {
			t.Errorf("activeKey = %q, want \"\" when no caches", activeKey)
		}
		for _, want := range mountPaths {
			if !slices.Contains(dirs, want) {
				t.Errorf("dirs = %v, want to contain %s", dirs, want)
			}
		}
	})

	t.Run("no_caches_no_mount_paths", func(t *testing.T) {
		active, dirs, activeKey := resolveCaches(nil, "/home/user", nil)
		if len(active) != 0 {
			t.Errorf("active = %v, want empty", active)
		}
		if len(dirs) != 0 {
			t.Errorf("dirs = %v, want empty", dirs)
		}
		if activeKey != "" {
			t.Errorf("activeKey = %q, want \"\"", activeKey)
		}
	})

	t.Run("shallow_copies_only_files", func(t *testing.T) {
		cacheDir := t.TempDir()
		// Create top-level files and a subdirectory with a file.
		if err := os.WriteFile(filepath.Join(cacheDir, "debug.keystore"), []byte("ks"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "adbkey"), []byte("key"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cacheDir, "avd", "Pixel_8"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "avd", "Pixel_8", "config.ini"), []byte("big"), 0o644); err != nil {
			t.Fatal(err)
		}

		caches := []CacheMount{{
			Name:          "android-keys",
			HostPath:      cacheDir,
			ContainerPath: "/home/user/.android",
			Shallow:       true,
		}}
		active, _, activeKey := resolveCaches(caches, "/home/user", nil)

		if len(active) != 1 {
			t.Fatalf("active = %d, want 1", len(active))
		}
		if activeKey == "" {
			t.Error("activeKey should be non-empty")
		}
		// Only top-level files, not subdirectory contents.
		got := active[0].files
		if len(got) != 2 {
			t.Fatalf("files = %v, want 2 entries", got)
		}
		for _, want := range []string{"adbkey", "debug.keystore"} {
			if !slices.Contains(got, want) {
				t.Errorf("files = %v, want to contain %s", got, want)
			}
		}
	})

	t.Run("shallow_skipped_when_no_files", func(t *testing.T) {
		cacheDir := t.TempDir()
		// Only a subdirectory, no top-level files.
		if err := os.MkdirAll(filepath.Join(cacheDir, "avd"), 0o755); err != nil {
			t.Fatal(err)
		}

		caches := []CacheMount{{
			Name:          "android-keys",
			HostPath:      cacheDir,
			ContainerPath: "/home/user/.android",
			Shallow:       true,
		}}
		active, _, _ := resolveCaches(caches, "/home/user", nil)
		if len(active) != 0 {
			t.Errorf("active = %d, want 0 (no top-level files)", len(active))
		}
	})

	t.Run("activeKey_differs_from_requested_when_dir_missing", func(t *testing.T) {
		requested := []CacheMount{{
			Name:          "missing",
			HostPath:      "/nonexistent/path",
			ContainerPath: "/home/user/.cache/missing",
		}}
		_, _, activeKey := resolveCaches(requested, "/home/user", nil)
		requestedKey := cacheSpecKey(requested)
		if activeKey == requestedKey {
			t.Errorf("activeKey %q should differ from requestedKey %q when host dir is missing", activeKey, requestedKey)
		}
	})
}

func TestGenerateDockerfile(t *testing.T) {
	t.Run("no_caches_no_dirs", func(t *testing.T) {
		got := generateDockerfile("mybase:latest", nil, nil, "sha256:abc", "ctxsha", "", "")
		if !strings.Contains(got, "FROM mybase:latest\n") {
			t.Error("missing FROM line")
		}
		if !strings.Contains(got, "COPY --chown=root:root ssh_host_ed25519_key") {
			t.Error("missing SSH key COPY")
		}
		if !strings.Contains(got, `LABEL md.base_digest="sha256:abc"`) {
			t.Errorf("missing base_digest label in:\n%s", got)
		}
		if strings.Contains(got, "mkdir") {
			t.Error("should not contain mkdir when dirs is empty")
		}
		if !strings.Contains(got, `CMD ["/root/start.sh"]`) {
			t.Error("missing CMD")
		}
	})

	t.Run("recursive_cache", func(t *testing.T) {
		active := []activeCM{{
			cm: CacheMount{Name: "go-mod", ContainerPath: "/home/user/go/pkg/mod"},
		}}
		got := generateDockerfile("base:v1", active, []string{"/home/user/go/pkg/mod"}, "", "", "cachekey", "")
		if !strings.Contains(got, `COPY --from=cache-go-mod --chown=user:user [".", "/home/user/go/pkg/mod/"]`) {
			t.Errorf("missing recursive COPY in:\n%s", got)
		}
		if !strings.Contains(got, "mkdir -p /home/user/go/pkg/mod") {
			t.Errorf("missing mkdir in:\n%s", got)
		}
	})

	t.Run("shallow_cache", func(t *testing.T) {
		active := []activeCM{{
			cm:    CacheMount{Name: "android-keys", ContainerPath: "/home/user/.android"},
			files: []string{"debug.keystore", "adbkey"},
		}}
		got := generateDockerfile("base:v1", active, nil, "", "", "", "")
		if !strings.Contains(got, `COPY --from=cache-android-keys --chown=user:user ["debug.keystore", "/home/user/.android/"]`) {
			t.Errorf("missing shallow COPY for debug.keystore in:\n%s", got)
		}
		if !strings.Contains(got, `COPY --from=cache-android-keys --chown=user:user ["adbkey", "/home/user/.android/"]`) {
			t.Errorf("missing shallow COPY for adbkey in:\n%s", got)
		}
	})

	t.Run("filename_with_spaces", func(t *testing.T) {
		active := []activeCM{{
			cm:    CacheMount{Name: "keys", ContainerPath: "/home/user/.keys"},
			files: []string{"my key.pem"},
		}}
		got := generateDockerfile("base:v1", active, nil, "", "", "", "")
		// JSON form should properly quote the filename.
		if !strings.Contains(got, `"my key.pem"`) {
			t.Errorf("filename with spaces not properly quoted in:\n%s", got)
		}
	})

	t.Run("dir_with_spaces", func(t *testing.T) {
		dirs := []string{"/home/user/my cache"}
		got := generateDockerfile("base:v1", nil, dirs, "", "", "", "")
		if !strings.Contains(got, "'/home/user/my cache'") {
			t.Errorf("dir with spaces not shell-quoted in:\n%s", got)
		}
	})

	t.Run("labels_set", func(t *testing.T) {
		got := generateDockerfile("img", nil, nil, "dig", "ctx", "ckey", "mdig")
		for _, want := range []string{
			`LABEL md.base_digest="dig"`,
			`LABEL md.context_sha="ctx"`,
			`LABEL md.cache_key="ckey"`,
			`LABEL md.base_manifest_digest="mdig"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})
}

func TestIsExecutable(t *testing.T) {
	// Walk the embedded rsc filesystem and verify that the executable-bit
	// heuristic (suffix .sh/xstartup, path contains /bin/, or shebang #!)
	// covers the files we expect to be executable.
	execFiles := []string{}
	err := fs.WalkDir(rscFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := rscFS.ReadFile(path)
		if err != nil {
			return err
		}
		if isExecutable(data) {
			execFiles = append(execFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(execFiles)

	// Verify expected executable files are present.
	wantExec := []string{
		"rsc/root/root/setup/1_packages.sh",
		"rsc/root/root/setup/2_neovim.sh",
		"rsc/root/root/setup/3_extrepo.sh",
		"rsc/root/root/setup/4_create_user.sh",
		"rsc/root/root/setup/5_kvm.sh",
		"rsc/root/root/setup/6_radare2.sh",
		"rsc/root/root/setup/7_podman.sh",
		"rsc/root/root/start.sh",
		"rsc/root/root/vnc-start.sh",
		"rsc/root/root/xfce-monitor.sh",
		"rsc/root/root/xvnc-monitor.sh",
		"rsc/root/usr/local/bin/git-credential-github",
		"rsc/root/usr/local/bin/google-chrome-stable",
		"rsc/root/usr/local/bin/measure_exec.sh",
		"rsc/user/home/user/setup/1_go.sh",
		"rsc/user/home/user/setup/2_nodejs.sh",
		"rsc/user/home/user/setup/3_bun.sh",
		"rsc/user/home/user/setup/4_android.sh",
		"rsc/user/home/user/setup/5_rust.sh",
		"rsc/user/home/user/setup/6_python.sh",
		"rsc/user/home/user/setup/7_llm_tools.sh",
		"rsc/user/home/user/setup/bashrc_cleanup.sh",
		"rsc/user/home/user/setup/generate_version_report.sh",
		"rsc/user/home/user/.vnc/xstartup",
	}
	slices.Sort(wantExec)
	if slices.Compare(execFiles, wantExec) != 0 {
		t.Errorf("executable files not as expected")
		t.Logf("Executable files: %d", len(execFiles))
		for _, f := range execFiles {
			t.Logf("  %s", f)
		}
		t.Logf("Expected files: %d", len(wantExec))
		for _, f := range wantExec {
			t.Logf("  %s", f)
		}
	}
}

func TestConvertGitURLToHTTPS(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/user/repo.git", "https://github.com/user/repo.git"},
		{"git_at", "git@github.com:user/repo.git", "https://github.com/user/repo.git"},
		{"ssh_git", "ssh://git@github.com/user/repo.git", "https://github.com/user/repo.git"},
		{"git_proto", "git://github.com/user/repo.git", "https://github.com/user/repo.git"},
		{"unknown", "unknown://foo", "unknown://foo"},
		{"whitespace", "  git@github.com:user/repo.git  ", "https://github.com/user/repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := convertGitURLToHTTPS(tt.in); got != tt.want {
				t.Errorf("convertGitURLToHTTPS(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
