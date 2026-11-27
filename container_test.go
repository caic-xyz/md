// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for container types, lifecycle, and Git synchronization.

package md

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/md/containers"
	"github.com/caic-xyz/md/git"
)

type cpuLimitRuntime struct {
	containers.Runtime

	cpus              int
	memTotal          uint64
	memoryUnsupported bool
	cpuErr            error
	args              []string
}

func (r *cpuLimitRuntime) Name() string { return "docker" }

func (r *cpuLimitRuntime) Info(context.Context) (containers.Info, error) {
	return containers.Info{CPUs: r.cpus, MemTotal: r.memTotal, MemoryLimit: !r.memoryUnsupported}, r.cpuErr
}

func (r *cpuLimitRuntime) RunOut(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
	r.args = slices.Clone(args)
	return errors.New("stop after recording launch")
}

// tailscaleStatusRuntime answers `tailscale status --json` with a fixed reply.
type tailscaleStatusRuntime struct {
	containers.Runtime

	status string
	err    error
	args   []string
}

func (r *tailscaleStatusRuntime) Run(_ context.Context, _ string, args ...string) (string, error) {
	r.args = slices.Clone(args)
	return r.status, r.err
}

func TestTailscaleNode(t *testing.T) {
	t.Parallel()
	// Trimmed from `tailscale status --json` in a running container.
	const status = `{"Version":"1.90.6","BackendState":"Running","Self":{"ID":"nTEST1CNTRL","HostName":"md-test","DNSName":"md-test.example.ts.net.","Online":true}}`
	for _, tc := range []struct {
		name      string
		tailscale bool
		state     string
		status    string
		err       error
		want      TailscaleNode
		wantExec  bool
	}{
		{name: "running", tailscale: true, state: "running", status: status, want: TailscaleNode{ID: "nTEST1CNTRL", FQDN: "md-test.example.ts.net"}, wantExec: true},
		{name: "stopped", tailscale: true, state: "exited", status: status},
		{name: "disabled", state: "running", status: status},
		{name: "exec_error", tailscale: true, state: "running", err: errors.New("tailscaled not running"), wantExec: true},
		{name: "bad_json", tailscale: true, state: "running", status: "{", wantExec: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &tailscaleStatusRuntime{status: tc.status, err: tc.err}
			c := &Container{Client: &Client{Runtime: r}, Logger: slog.New(slog.DiscardHandler), Name: "md-test", Tailscale: tc.tailscale, State: tc.state}
			if got := c.TailscaleNode(t.Context()); got != tc.want {
				t.Fatalf("TailscaleNode() = %+v; want %+v", got, tc.want)
			}
			if tc.wantExec != (len(r.args) > 0) {
				t.Fatalf("runtime args = %v; want exec %t", r.args, tc.wantExec)
			}
		})
	}
}

// tailscaleResumeRuntime answers `tailscale status --json` with successive
// backend states and records the login command.
type tailscaleResumeRuntime struct {
	containers.Runtime

	states   []string
	loginErr error
	login    []string
}

func (r *tailscaleResumeRuntime) Run(context.Context, string, ...string) (string, error) {
	state := r.states[0]
	if len(r.states) > 1 {
		r.states = r.states[1:]
	}
	return `{"BackendState":"` + state + `","Self":{"ID":"nTEST1CNTRL"}}`, nil
}

func (r *tailscaleResumeRuntime) RunOut(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
	r.login = slices.Clone(args)
	return r.loginErr
}

func TestResumeTailscale(t *testing.T) { //nolint:paralleltest // replaces the process-wide default HTTP transport.
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	const apiKey = "tskey-api-test" //nolint:gosec // G101: fake token
	for _, tc := range []struct {   //nolint:paralleltest // shares the process-wide default HTTP transport.
		name     string
		states   []string
		apiKey   string
		loginErr error
		tags     []string
		wantErr  string
		wantKey  bool
	}{
		{name: "running", states: []string{"Starting", "Running"}, apiKey: apiKey},
		{name: "node_removed", states: []string{"Starting", "NeedsLogin"}, apiKey: apiKey, wantKey: true},
		{name: "node_removed_with_saved_tags", states: []string{"NeedsLogin"}, apiKey: apiKey, tags: []string{"tag:ci"}, wantKey: true},
		{name: "node_removed_without_api_key", states: []string{"NeedsLogin"}, wantErr: "needs a Tailscale API key"},
		{name: "login_fails", states: []string{"NeedsLogin"}, apiKey: apiKey, loginErr: errors.New("exit status 1"), wantErr: "tailscale login: exit status 1", wantKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var tags [][]string
			http.DefaultTransport = &authKeyTransport{events: &events, tags: &tags}
			r := &tailscaleResumeRuntime{states: tc.states, loginErr: tc.loginErr}
			c := &Container{Client: &Client{Runtime: r}, Logger: slog.New(slog.DiscardHandler), Name: "md-test", Tailscale: true, TailscaleTags: tc.tags, State: "running"}
			err := c.resumeTailscale(t.Context(), io.Discard, io.Discard, tc.apiKey)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("resumeTailscale() = %v; want error containing %q", err, tc.wantErr)
			}
			if minted := slices.Contains(events, "key"); minted != tc.wantKey {
				t.Fatalf("minted auth key = %t; want %t", minted, tc.wantKey)
			}
			wantTags := tc.tags
			if wantTags == nil {
				wantTags = []string{DefaultTailscaleTag}
			}
			if tc.wantKey && (len(tags) != 1 || !slices.Equal(tags[0], wantTags)) {
				t.Fatalf("auth key tags = %q; want %q", tags, wantTags)
			}
			want := []string(nil)
			if tc.wantKey {
				want = []string{"exec", "--user", "0:0", "-e", "TAILSCALE_AUTHKEY=test-key", "md-test", "/root/tailscale-up.sh"}
			}
			if !slices.Equal(r.login, want) {
				t.Fatalf("login command = %v; want %v", r.login, want)
			}
		})
	}
}

func TestLaunchContainerTailscaleTags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{name: "default", want: "md.tailscale_tags=tag:md"},
		{name: "configured", tags: []string{"tag:ci", "tag:agents"}, want: "md.tailscale_tags=tag:ci tag:agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &cpuLimitRuntime{cpus: 8, memTotal: 16 << 30}
			c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
			opts := &StartOpts{TailscaleAPIKey: "tskey-api-test", TailscaleTags: tc.tags, tailscaleAuthKey: "tskey-auth-test"} //nolint:gosec // G101: fake tokens
			if err := c.createContainer(t.Context(), io.Discard, io.Discard, opts, "test-image"); err == nil {
				t.Fatal("expected recording runtime to stop launch")
			}
			if !slices.Contains(r.args, tc.want) {
				t.Fatalf("launch args = %v; want label %s", r.args, tc.want)
			}
		})
	}
}

func TestLaunchContainerCPULimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		cpus      int
		requested int
		want      string
	}{
		{name: "docker_desktop", cpus: 14, requested: DefaultMaxCPUs, want: "12"},
		{name: "single_cpu", cpus: 1, requested: DefaultMaxCPUs, want: "1"},
		{name: "two_cpus", cpus: 2, requested: DefaultMaxCPUs, want: "2"},
		{name: "three_cpus", cpus: 3, requested: DefaultMaxCPUs, want: "2"},
		{name: "explicit", requested: 4, want: "4"},
		{name: "unlimited", requested: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &cpuLimitRuntime{cpus: tc.cpus, memTotal: 16 << 30}
			c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
			if err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxCPUs: tc.requested}, "test-image"); err == nil {
				t.Fatal("expected recording runtime to stop launch")
			}
			i := slices.Index(r.args, "--cpus")
			if tc.want == "" && len(r.args) > 0 && i < 0 {
				return
			}
			if i < 0 || i+1 >= len(r.args) || r.args[i+1] != tc.want {
				t.Fatalf("launch args = %v; want --cpus %s", r.args, tc.want)
			}
		})
	}
}

func TestLaunchContainerMemoryLimit(t *testing.T) {
	t.Parallel()
	t.Run("unlimited", func(t *testing.T) {
		t.Parallel()
		for _, extra := range [][]string{nil, {"--memory=4g"}, {"--cgroups=disabled"}} {
			r := &cpuLimitRuntime{cpus: 8, memoryUnsupported: true}
			c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
			err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{ExtraRunArgs: extra}, "test-image")
			if err == nil || len(r.args) == 0 || slices.Contains(r.args, "--memory") {
				t.Fatalf("unlimited launch with unknown memory: err = %v, args = %v", err, r.args)
			}
			for _, arg := range extra {
				if !slices.Contains(r.args, arg) {
					t.Fatalf("extra arg %q lost: %v", arg, r.args)
				}
			}
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		r := &cpuLimitRuntime{cpus: 8}
		c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
		err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxMemory: 12 << 30}, "test-image")
		if err == nil {
			t.Fatal("expected recording runtime to stop launch")
		}
		i := slices.Index(r.args, "--memory")
		if i < 0 || i+1 >= len(r.args) || r.args[i+1] != "12884901888" {
			t.Fatalf("launch args = %v; want 12 GiB memory limit", r.args)
		}
	})
	t.Run("clustered_shorthand", func(t *testing.T) {
		t.Parallel()
		r := &cpuLimitRuntime{cpus: 8, memTotal: 16 << 30}
		c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
		err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxMemory: 12 << 30, ExtraRunArgs: []string{"-im0"}}, "test-image")
		if err == nil || !slices.Contains(r.args, "-im0") || len(r.args) < 3 || !slices.Equal(r.args[len(r.args)-3:], []string{"--memory", "12884901888", "test-image"}) {
			t.Fatalf("clustered shorthand: err = %v, launch args = %v", err, r.args)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, extra := range [][]string{{"--memory", "0"}, {"--memory=64g"}, {"-m0"}, {"-m", "0"}, {"--cgroups=disabled"}, {"--cgroups", "disabled"}, {"--cgroup-conf=memory.max=max"}, {"--cgroup-conf", "memory.max=max"}, {"--"}} {
			r := &cpuLimitRuntime{cpus: 8, memTotal: 16 << 30}
			c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
			err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxMemory: 12 << 30, ExtraRunArgs: extra}, "test-image")
			if err == nil || !strings.Contains(err.Error(), "conflicts") || len(r.args) != 0 {
				t.Fatalf("extra args %v: err = %v, launch args = %v", extra, err, r.args)
			}
		}
		r := &cpuLimitRuntime{cpus: 8, memTotal: 16 << 30, memoryUnsupported: true}
		c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
		err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxMemory: 12 << 30}, "test-image")
		if err == nil || !strings.Contains(err.Error(), "cannot enforce") || len(r.args) != 0 {
			t.Fatalf("unsupported cgroups: err = %v, launch args = %v", err, r.args)
		}
		for _, limit := range []uint64{1, 1024, 6*1024*1024 - 1} {
			r := &cpuLimitRuntime{cpus: 8}
			c := &Container{Client: &Client{Runtime: r}, Name: "md-test"}
			err := c.createContainer(t.Context(), io.Discard, io.Discard, &StartOpts{MaxMemory: limit}, "test-image")
			if err == nil || !strings.Contains(err.Error(), "at least 6 MiB") || len(r.args) != 0 {
				t.Fatalf("limit %d: err = %v, launch args = %v", limit, err, r.args)
			}
		}
	})
}

// canonicalTempDir resolves a temporary directory as Git resolves a worktree
// root. macOS temporary directories are behind a symlink and Windows ones may
// use 8.3 short names.
func canonicalTempDir(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func runTestGit(t *testing.T, ctx context.Context, wd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are from test code
	cmd.Dir = wd
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"LANG=C",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func TestRepositoryObjectCopy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if copied, err := seedContainerRepo(ctx, nil, &Repo{GitRoot: filepath.Join(t.TempDir(), "missing")}, -1, io.Discard, io.Discard); err != nil || copied {
		t.Fatalf("disabled object copy = %t, %v", copied, err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, ctx, "", "init", "-q", "-b", "main", src)
	runTestGit(t, ctx, src, "config", "user.name", "Object Copy Test")
	runTestGit(t, ctx, src, "config", "user.email", "object-copy@example.test")
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, ctx, src, "add", "file.txt")
	runTestGit(t, ctx, src, "commit", "-qm", "first")
	runTestGit(t, ctx, src, "repack", "-ad")
	snapshot, err := seedSource(ctx, src)
	if err != nil || snapshot.objectsDir == "" {
		t.Fatalf("source objects = %q, %v", snapshot.objectsDir, err)
	}
	worktree := filepath.Join(t.TempDir(), "linked")
	runTestGit(t, ctx, src, "worktree", "add", "-q", "-b", "linked", worktree)
	linked, err := seedSource(ctx, worktree)
	if err != nil || linked.objectsDir != snapshot.objectsDir {
		t.Fatalf("linked worktree objects = %q, %v; want %q", linked.objectsDir, err, snapshot.objectsDir)
	}

	dst := filepath.Join(t.TempDir(), "destination")
	runTestGit(t, ctx, "", "init", "-q", dst)
	var archive bytes.Buffer
	if err := writeSeedTar(&archive, snapshot.objectsDir, snapshot.refs); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dst, ".git", filepath.FromSlash(h.Name))
		if h.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		f, err := os.Create(path) //nolint:gosec // archive is generated from this test's seed
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(f, tr) //nolint:gosec // trusted small test archive
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	destinationRefs := runTestGit(t, ctx, dst, "for-each-ref", "--format=%(refname)")
	if !strings.Contains(destinationRefs, "refs/md/seed/heads/main") || strings.Contains(destinationRefs, "refs/heads/main") {
		t.Fatalf("seed refs = %q", destinationRefs)
	}
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, ctx, src, "commit", "-qam", "second")
	push := exec.CommandContext(ctx, "git", "push", "-q", dst, "HEAD:refs/heads/main") //nolint:gosec // test-controlled args
	push.Dir = src
	push.Env = append(os.Environ(), "GIT_TRACE_PACKET=1")
	packetTrace, err := push.CombinedOutput()
	if err != nil {
		t.Fatalf("push after object copy: %v: %s", err, packetTrace)
	}
	if !bytes.Contains(packetTrace, []byte("refs/md/seed/heads/main")) {
		t.Fatalf("push did not see the seed ref: %s", packetTrace)
	}
	if got, want := runTestGit(t, ctx, dst, "rev-parse", "main"), runTestGit(t, ctx, src, "rev-parse", "HEAD"); got != want {
		t.Fatalf("pushed main = %q, want %q", got, want)
	}
	runTestGit(t, ctx, dst, "fsck", "--connectivity-only", "--no-reflogs")
}

func TestProvisionRepositoriesCapsConcurrency(t *testing.T) { //nolint:paralleltest // Shares the package-global provisioning semaphore.
	const repoCount = maxConcurrentRepoProvisioning + 8
	var active atomic.Int32
	var peak atomic.Int32
	started := make(chan struct{}, repoCount)
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- provisionRepositories(t.Context(), repoCount, func(ctx context.Context, _ int) error {
			current := active.Add(1)
			for {
				previous := peak.Load()
				if current <= previous || peak.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				active.Add(-1)
				return nil
			case <-ctx.Done():
				active.Add(-1)
				return ctx.Err()
			}
		})
	}()

	for range maxConcurrentRepoProvisioning {
		<-started
	}
	if got := active.Load(); got != maxConcurrentRepoProvisioning {
		t.Fatalf("active provisioning operations = %d, want %d", got, maxConcurrentRepoProvisioning)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != maxConcurrentRepoProvisioning {
		t.Fatalf("peak provisioning operations = %d, want %d", got, maxConcurrentRepoProvisioning)
	}
}

func TestProvisionRepositoriesCapsConcurrencyAcrossContainers(t *testing.T) { //nolint:paralleltest // Shares the package-global provisioning semaphore.
	var active atomic.Int32
	var peak atomic.Int32
	started := make(chan struct{}, 2*maxConcurrentRepoProvisioning)
	release := make(chan struct{})
	done := make(chan error, 2)
	provision := func(ctx context.Context, _ int) error {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			active.Add(-1)
			return nil
		case <-ctx.Done():
			active.Add(-1)
			return ctx.Err()
		}
	}
	for range 2 {
		go func() {
			done <- provisionRepositories(t.Context(), maxConcurrentRepoProvisioning, provision)
		}()
	}

	for range maxConcurrentRepoProvisioning {
		<-started
	}
	if got := active.Load(); got != maxConcurrentRepoProvisioning {
		t.Fatalf("active provisioning operations = %d, want %d", got, maxConcurrentRepoProvisioning)
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := peak.Load(); got != maxConcurrentRepoProvisioning {
		t.Fatalf("peak provisioning operations = %d, want %d", got, maxConcurrentRepoProvisioning)
	}
}

func BenchmarkProvisionRepositories17(b *testing.B) {
	b.Run("dispatch", func(b *testing.B) {
		b.ReportAllocs()
		ctx := b.Context()
		for b.Loop() {
			if err := provisionRepositories(ctx, 17, func(context.Context, int) error {
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("simulated_io", func(b *testing.B) {
		b.ReportAllocs()
		ctx := b.Context()
		for b.Loop() {
			if err := provisionRepositories(ctx, 17, func(context.Context, int) error {
				time.Sleep(time.Millisecond)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func sameTestPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func writeTestFile(t *testing.T, name, content string) {
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil { //nolint:gosec // test data, world-readable is fine
		t.Fatal(err)
	}
}

func writeTestSSHConfig(t *testing.T, home string) {
	sshConfigDir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshConfigDir, "md-test.conf"), []byte("Host md-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupPullTest(t *testing.T) (ct *Container, hostDir, containerDir string) {
	ctx := t.Context()
	fakeSSH(t)
	home := t.TempDir()
	writeTestSSHConfig(t, home)

	originDir := filepath.Join(t.TempDir(), "origin.git")
	hostDir = t.TempDir()
	containerDir = t.TempDir()
	runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
	runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
	runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
	runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
	runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
	runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
	runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v1\n")
	runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container v1")
	runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

	logger := testLogger(t)
	ct = &Container{
		Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
		Logger: logger,
		Name:   "md-test",
		Repos: []Repo{{
			GitRoot:       hostDir,
			Branches:      []string{"main"},
			ContainerPath: filepath.ToSlash(containerDir),
			DefaultRemote: "origin",
			DefaultBranch: "main",
		}},
	}
	if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
		t.Fatalf("initial Pull: %v", err)
	}
	return ct, hostDir, containerDir
}

func setupMultiBranchPullTest(t *testing.T) (ct *Container, hostDir, containerDir string) {
	ctx := t.Context()
	fakeSSH(t)
	home := t.TempDir()
	writeTestSSHConfig(t, home)

	originDir := filepath.Join(t.TempDir(), "origin.git")
	hostDir = t.TempDir()
	containerDir = t.TempDir()
	runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
	runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
	runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(hostDir, "main.txt"), "main base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main base")
	runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
	runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "feature")
	writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature base\n")
	runTestGit(t, ctx, hostDir, "add", ".")
	runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature base")
	runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")
	runTestGit(t, ctx, hostDir, "switch", "-q", "main")

	runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
	runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
	runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
	runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

	logger := testLogger(t)
	ct = &Container{
		Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
		Logger: logger,
		Name:   "md-test",
		Repos: []Repo{{
			GitRoot:       hostDir,
			Branches:      []string{"main", "feature"},
			ContainerPath: filepath.ToSlash(containerDir),
			DefaultRemote: "origin",
			DefaultBranch: "main",
		}},
	}
	return ct, hostDir, containerDir
}

// installRejectingHooks installs pre-commit and commit-msg hooks that always
// fail, so a container commit succeeds only when Git skips them.
func installRejectingHooks(t *testing.T, repoDir string) {
	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "commit-msg"} {
		path := filepath.Join(hooksDir, name)
		writeTestFile(t, path, "#!/bin/sh\necho rejected by "+name+" >&2\nexit 1\n")
		// #nosec G302 -- Git requires hooks to be executable.
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			if got := shellQuote(tt.in); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShellQuoteArgs(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got := shellQuoteArgs([]string{"printf", "%s\n", "hello world", "$(not-run)", "it's"})
		want := `printf '%s
' 'hello world' '$(not-run)' 'it'\''s'`
		if got != want {
			t.Errorf("shellQuoteArgs() = %q, want %q", got, want)
		}
	})
}

func TestRenderExtraEnv(t *testing.T) {
	t.Parallel()
	got, err := renderExtraEnv([]string{"MULTI=line 1\nline 2", "QUOTE=a'b", "GONE="})
	if err != nil {
		t.Fatal(err)
	}
	want := "MULTI='line 1\nline 2'\nQUOTE='a'\\''b'\nunset GONE\n"
	if string(got) != want {
		t.Errorf("renderExtraEnv() = %q, want %q", got, want)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", string(got)+`printf '%s' "$MULTI"`) //nolint:gosec // script is generated from test literals.
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "line 1\nline 2" {
		t.Errorf("sourced MULTI = %q", out)
	}
}

func TestCheckRepoOverlap(t *testing.T) {
	t.Parallel()
	name := "md-genai-caic-5"
	repos := []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}}}
	t.Run("no existing containers", func(t *testing.T) {
		t.Parallel()
		if err := checkRepoOverlap(name, repos, nil); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("distinct repo", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  "md-caic-caic-2",
			Repos: []Repo{{GitRoot: "/home/user/src/caic", Branches: []string{"caic-2"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("distinct branch, same repo", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  "md-genai-main",
			Repos: []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"main"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("same name is not a conflict with itself", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name:  name,
			Repos: []Repo{{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}}},
		}}
		if err := checkRepoOverlap(name, repos, existing); err != nil {
			t.Fatalf("checkRepoOverlap() = %v, want nil", err)
		}
	})
	t.Run("overlapping repo and branch as an extra repo in another container", func(t *testing.T) {
		t.Parallel()
		existing := []*Container{{
			Name: "md-caic-caic-2",
			Repos: []Repo{
				{GitRoot: "/home/user/src/caic", Branches: []string{"caic-2"}},
				{GitRoot: "/home/user/src/genai", Branches: []string{"caic-5"}},
			},
		}}
		err := checkRepoOverlap(name, repos, existing)
		if err == nil {
			t.Fatal("checkRepoOverlap() = nil, want error")
		}
		if !strings.Contains(err.Error(), "md-caic-caic-2") {
			t.Fatalf("checkRepoOverlap() = %v, want mention of md-caic-caic-2", err)
		}
	})
}

// setupDiffTestRepo creates a repository on branch main with one commit,
// tracking host/main as the container's mapped branches do.
func setupDiffTestRepo(t *testing.T, ctx context.Context) string {
	dir := t.TempDir()
	runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
	runTestGit(t, ctx, dir, "config", "user.name", "Test")
	runTestGit(t, ctx, dir, "config", "user.email", "test@test")
	writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
	runTestGit(t, ctx, dir, "add", ".")
	runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
	runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
	runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
	runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
	runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")
	return dir
}

// runTestDiffCommand runs the generated container diff command and returns its
// trimmed stdout and its stderr.
func runTestDiffCommand(t *testing.T, ctx context.Context, req *diffRequest) (stdout, stderr string) {
	cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(req)) //nolint:gosec // repo path is a test temp dir
	cmd.Env = append(os.Environ(), "LANG=C")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	return strings.TrimSpace(out.String()), errOut.String()
}

func fakeGitDiffOnce(t *testing.T, vanishPath string) []string {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	t.Cleanup(func() {
		if err := removeAllWithRetry(binDir); err != nil {
			t.Errorf("removing fake Git dir %q: %v", binDir, err)
		}
	})
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := linkOrCopyExecutable(executable, filepath.Join(binDir, name)); err != nil {
		t.Fatal(err)
	}
	return []string{
		fakeGitDiffFailureEnv + "=1",
		fakeGitExecutableEnv + "=" + realGit,
		fakeGitMarkerEnv + "=" + filepath.Join(t.TempDir(), "failed"),
		fakeGitVanishPathEnv + "=" + vanishPath,
		"LANG=C",
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

// gitShimPATH writes a git wrapper that runs inject before delegating to the
// real git, and returns a PATH value that puts the wrapper first.
func gitShimPATH(t *testing.T, inject string) string {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + inject + "\nexec " + shellQuote(gitPath) + ` "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // the wrapper must be executable.
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestDiff(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "old\n")
		writeTestFile(t, filepath.Join(dir, "staged.txt"), "old\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "init")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "new\n")
		writeTestFile(t, filepath.Join(dir, "staged.txt"), "new\n")
		runTestGit(t, ctx, dir, "add", "staged.txt")
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "new\n")

		diffCommand := gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main"})
		if strings.Count(diffCommand, "git diff ") != 1 {
			t.Fatalf("diff command runs multiple git diff invocations: %s", diffCommand)
		}
		if strings.Contains(diffCommand, "git diff --no-index") {
			t.Fatalf("diff command uses separate no-index diff invocation: %s", diffCommand)
		}

		cmd := exec.CommandContext(ctx, "bash", "-c", diffCommand) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		out := stdout.String()
		for _, name := range []string{"tracked.txt", "staged.txt", "untracked.txt"} {
			if !strings.Contains(out, name) {
				t.Errorf("diff output missing %q:\n%s", name, out)
			}
		}
		if got := runTestGit(t, ctx, dir, "diff", "--cached", "--name-only"); got != "staged.txt" {
			t.Errorf("cached diff = %q, want staged.txt", got)
		}
		status := runTestGit(t, ctx, dir, "status", "--short")
		for _, want := range []string{" M tracked.txt", "M  staged.txt", "?? untracked.txt"} {
			if !strings.Contains(status, want) {
				t.Errorf("status missing %q:\n%s", want, status)
			}
		}
	})
	t.Run("valid_retries_when_a_file_vanishes", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		vanishedPath := filepath.Join(dir, "vanished.txt")
		writeTestFile(t, vanishedPath, "vanished\n")
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), fakeGitDiffOnce(t, vanishedPath)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), "vanished.txt") {
			t.Errorf("diff --stat output contains vanished.txt:\n%s", stdout.String())
		}
		if strings.Contains(stderr.String(), "fatal: stat") {
			t.Errorf("diff leaked the recovered transient error:\n%s", stderr.String())
		}
	})
	t.Run("valid_skips_an_untracked_file_that_vanishes_before_add", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		vanishedPath := filepath.Join(dir, "vanished.txt")
		writeTestFile(t, vanishedPath, "vanished\n")
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")

		shim := gitShimPATH(t, `if [ "$1" = add ] && [ ! -e "$MD_TEST_ADD_MARKER" ]; then
	: > "$MD_TEST_ADD_MARKER"
	rm -f `+shellQuote(vanishedPath)+`
fi`)
		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C", "PATH="+shim, "MD_TEST_ADD_MARKER="+filepath.Join(t.TempDir(), "marker"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), "vanished.txt") || strings.Contains(stderr.String(), "did not match any files") {
			t.Errorf("skipped add failure leaked:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
	})
	t.Run("valid_ignores_an_untracked_nested_repository", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "retained.txt"), "retained\n")
		nested := filepath.Join(dir, "nested")
		if err := os.Mkdir(nested, 0o750); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, ctx, nested, "init", "-q", "--initial-branch=main")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})
		if !strings.Contains(stdout, "retained.txt") {
			t.Errorf("diff --stat output missing retained.txt:\n%s", stdout)
		}
		if strings.Contains(stderr, "adding files failed") || strings.Contains(stdout, "nested/") {
			t.Errorf("nested repository leaked into the diff:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})
	t.Run("error_does_not_retry_unrelated_git_failure", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--stat"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), fakeGitDiffOnce(t, "")...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("diff command unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "fatal: simulated diff failure") {
			t.Errorf("diff did not surface the original Git failure:\n%s", stderr.String())
		}
	})
	t.Run("valid_uses_merge_base_when_upstream_moves", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		writeTestFile(t, filepath.Join(dir, "local.txt"), "local\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "local")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "upstream-update", baseCommit)
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		upstreamCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", upstreamCommit)
		runTestGit(t, ctx, dir, "checkout", "-q", "main")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}})
		if !strings.Contains(stdout, "local.txt") {
			t.Errorf("diff output missing local.txt:\n%s", stdout)
		}
		if strings.Contains(stdout, "upstream.txt") {
			t.Errorf("diff output includes updated upstream file:\n%s", stdout)
		}
		if !strings.Contains(stderr, `branch "main" has no recorded host integration point`) {
			t.Errorf("stderr missing the integration-point note:\n%s", stderr)
		}
	})
	t.Run("valid_since_sync_point", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "synced.txt"), "synced\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "synced")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")

		writeTestFile(t, filepath.Join(dir, "committed.txt"), "committed\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "after the sync point")
		writeTestFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if want := "committed.txt\nuntracked.txt"; stdout != want {
			t.Errorf("diff --name-only = %q, want %q\nstderr:\n%s", stdout, want, stderr)
		}
		req.full = true
		stdout, stderr = runTestDiffCommand(t, ctx, req)
		if want := "committed.txt\nsynced.txt\nuntracked.txt"; stdout != want {
			t.Errorf("full diff --name-only = %q, want %q\nstderr:\n%s", stdout, want, stderr)
		}
	})
	t.Run("valid_since_sync_point_survives_rebase_and_gc", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "work")
		syncCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", syncCommit)

		// The upstream gains a commit and the agent rebases onto it, which makes
		// the recorded sync point unreachable from every branch.
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "upstream-update", baseCommit)
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		runTestGit(t, ctx, dir, "branch", "-q", "-D", "upstream-update")
		runTestGit(t, ctx, dir, "rebase", "-q", "host/main")
		runTestGit(t, ctx, dir, "update-ref", "-d", "ORIG_HEAD")
		runTestGit(t, ctx, dir, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")
		runTestGit(t, ctx, dir, "gc", "-q", "--prune=now")

		// Only the sync ref keeps the base of the diff alive.
		runTestGit(t, ctx, dir, "cat-file", "-e", syncCommit)
		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		// The rebase replayed work.txt unchanged, so the diff from the sync point
		// holds the upstream commit only.
		if stdout != "upstream.txt" {
			t.Errorf("diff --name-only = %q, want upstream.txt", stdout)
		}
		if !strings.Contains(stderr, "moved onto a newer upstream") {
			t.Errorf("stderr missing the rebase note:\n%s", stderr)
		}
	})
	t.Run("valid_since_sync_point_during_a_rebase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")
		writeTestFile(t, filepath.Join(dir, "synced.txt"), "synced\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "work")
		// The sync point is the work commit, which already holds synced.txt. A
		// diff from it therefore reports only the conflicted file, while the
		// upstream merge base would also report synced.txt as added.
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")

		// Conflict with the rebase target so the rebase stops with HEAD detached.
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "rebase-target", "HEAD~1")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "target\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		rebase := exec.CommandContext(ctx, "git", "rebase", "rebase-target")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "" {
			t.Fatalf("branch --show-current = %q, want empty during the rebase", got)
		}

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "work.txt" {
			t.Errorf("diff --name-only = %q, want work.txt only, so the sync point was the base\nstderr:\n%s", stdout, stderr)
		}
		if strings.Contains(stderr, "no recorded host integration point") {
			t.Errorf("stderr claims no integration point while a rebase is in progress:\n%s", stderr)
		}
	})
	t.Run("valid_reset_behind_the_sync_point_omits_the_upstream_note", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		baseCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		writeTestFile(t, filepath.Join(dir, "upstream.txt"), "upstream\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "upstream")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"main", "HEAD")
		// The agent drops the upstream commit instead of moving onto a new one.
		runTestGit(t, ctx, dir, "reset", "--hard", "-q", baseCommit)

		req := &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "upstream.txt" {
			t.Errorf("diff --name-only = %q, want upstream.txt\nstderr:\n%s", stdout, stderr)
		}
		if strings.Contains(stderr, "moved onto a newer upstream") {
			t.Errorf("stderr claims the branch moved onto a newer upstream:\n%s", stderr)
		}
	})
	t.Run("valid_quiet_omits_base_notes", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := setupDiffTestRepo(t, ctx)
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")

		stdout, stderr := runTestDiffCommand(t, ctx, &diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--quiet"}, quiet: true})
		if stdout != "" || stderr != "" {
			t.Errorf("quiet diff = stdout %q, stderr %q; want both empty", stdout, stderr)
		}
	})
	t.Run("valid_since_sync_point_without_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "base.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"caic-1", "HEAD")
		writeTestFile(t, filepath.Join(dir, "work.txt"), "work\n")

		req := &diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main", extraArgs: []string{"--name-only"}}
		stdout, stderr := runTestDiffCommand(t, ctx, req)
		if stdout != "work.txt" {
			t.Errorf("diff --name-only = %q, want work.txt\nstderr:\n%s", stdout, stderr)
		}
	})
	t.Run("valid_legacy_base_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "old\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "init")
		runTestGit(t, ctx, dir, "branch", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-B", "caic-2", "base")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=base", "caic-2")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "new\n")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-2", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if out := stdout.String(); !strings.Contains(out, "tracked.txt") {
			t.Errorf("diff output missing tracked.txt:\n%s", out)
		}
	})
	t.Run("valid_rebase_in_progress", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/host/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.host.fetch", "+refs/remotes/host/*:refs/remotes/host/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=host/main", "main")

		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "rebase-target")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "target\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local")

		rebase := exec.CommandContext(ctx, "git", "rebase", "rebase-target")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "rebase-merge", "head-name")); err != nil {
			t.Fatalf("rebase metadata missing: %v", err)
		}

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "main", defaultRemote: "host", defaultBranch: "main", extraArgs: []string{"--name-only"}})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("diff command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != "tracked.txt" {
			t.Errorf("diff --name-only = %q, want tracked.txt", got)
		}
	})
	t.Run("error_checked_out_branch_has_no_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.origin.url", ".")
		runTestGit(t, ctx, dir, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
		runTestGit(t, ctx, dir, "branch", "-q", "--set-upstream-to=origin/main", "caic-1")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "fix-$(touch-pwn)")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			`current branch "fix-$(touch-pwn)" has no upstream branch configured`,
			`The container's primary branch is "caic-1".`,
			"git switch caic-1",
			"git branch --set-upstream-to=caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
		if strings.Contains(stderr.String(), "git branch --set-upstream-to=caic-1 fix-$(touch-pwn)") {
			t.Fatalf("error suggests an unsafe command:\n%s", stderr.String())
		}
	})
	t.Run("error_primary_branch_has_no_upstream", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			`primary branch "caic-1" has no upstream branch configured`,
			"git branch --set-upstream-to=origin/main caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
	})
	t.Run("error_detached_head_suggests_primary_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-1")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")
		runTestGit(t, ctx, dir, "switch", "-q", "--detach")

		cmd := exec.CommandContext(ctx, "bash", "-c", gitDiffCommand(&diffRequest{repo: dir, primaryBranch: "caic-1", defaultRemote: "origin", defaultBranch: "main"})) //nolint:gosec // repo path is a test temp dir
		cmd.Env = append(os.Environ(), "LANG=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("diff command unexpectedly succeeded")
		}
		for _, want := range []string{
			"HEAD is detached",
			"git switch caic-1",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("error missing %q:\n%s", want, stderr.String())
			}
		}
	})
}

func TestPlanFork(t *testing.T) {
	t.Parallel()
	source := []Repo{
		{GitRoot: "/src/a", Branches: []string{"main", "feat"}},
		{GitRoot: "/src/b", Branches: []string{"main"}},
	}

	t.Run("orders_source_then_extras", func(t *testing.T) {
		t.Parallel()
		// Extras keep caller order; source dests follow source order regardless
		// of where they appear in the spec.
		ctx := t.Context()
		xDir := t.TempDir()
		xRemote := filepath.Join(t.TempDir(), "x.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=dev", xRemote)
		runTestGit(t, ctx, xDir, "init", "-q", "--initial-branch=dev")
		runTestGit(t, ctx, xDir, "commit", "-q", "--allow-empty", "-m", "dev")
		runTestGit(t, ctx, xDir, "remote", "add", "origin", xRemote)
		runTestGit(t, ctx, xDir, "remote", "add", "md-other", "user@md-other:/home/user/src/"+filepath.Base(xDir))
		runTestGit(t, ctx, xDir, "push", "-q", "-u", "origin", "dev")
		runTestGit(t, ctx, xDir, "config", "branch.dev.pushRemote", "md-other")
		runTestGit(t, ctx, xDir, "remote", "set-head", "origin", "dev")
		yDir := t.TempDir()
		yRemote := filepath.Join(t.TempDir(), "y.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", yRemote)
		runTestGit(t, ctx, yDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, yDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, yDir, "branch", "topic")
		runTestGit(t, ctx, yDir, "branch", "support")
		runTestGit(t, ctx, yDir, "remote", "add", "origin", yRemote)
		runTestGit(t, ctx, yDir, "push", "-q", "-u", "origin", "main", "topic")
		runTestGit(t, ctx, yDir, "branch", "--set-upstream-to=support", "topic")
		spec := []ForkRepo{
			{GitRoot: xDir, SourceBranches: []string{"dev"}, TagRegexp: "^v", DestPrimary: "x0"},
			{GitRoot: "/src/b", SourceBranches: []string{"main"}, DestPrimary: "b0"},
			{GitRoot: "/src/a", DestPrimary: "a0"}, // SourceBranches omitted: allowed for a source repo.
			{GitRoot: yDir, SourceBranches: []string{"main", "topic"}, ContainerPath: "/home/user/src/nested/y", DestPrimary: "y0"},
		}
		plan, err := planFork(ctx, testLogger(t), "", source, spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.extraRepos[1].ContainerPath; got != "/home/user/src/nested/y" {
			t.Errorf("extra /src/y ContainerPath = %q, want /home/user/src/nested/y", got)
		}
		if want := []string{"a0", "b0"}; !slices.Equal(plan.srcDest, want) {
			t.Errorf("srcDest = %v, want %v", plan.srcDest, want)
		}
		if want := []string{"x0", "y0"}; !slices.Equal(plan.extraDest, want) {
			t.Errorf("extraDest = %v, want %v", plan.extraDest, want)
		}
		if len(plan.extraRepos) != 2 || plan.extraRepos[0].GitRoot != xDir || plan.extraRepos[1].GitRoot != yDir {
			t.Fatalf("extraRepos = %+v, want x then y", plan.extraRepos)
		}
		if plan.extraRepos[0].TagRegexp != "^v" {
			t.Errorf("extra x TagRegexp = %q, want ^v", plan.extraRepos[0].TagRegexp)
		}
		if want := []string{"origin"}; !slices.Equal(plan.extraRepos[0].Remotes, want) {
			t.Errorf("extra x remotes = %v, want %v", plan.extraRepos[0].Remotes, want)
		}
		destinations := []Repo{{Branches: []string{"x0"}}, {Branches: []string{"y0", "topic"}}}
		preflight, err := preflightForkRepos(ctx, testLogger(t), nil, plan.extraRepos, destinations)
		if err != nil {
			t.Fatal(err)
		}
		if base := preflight.extraBases[0][0]; base.pushRemote != "" {
			t.Errorf("extra x push remote = %q, want empty", base.pushRemote)
		}
		if want := []string{"main", "topic"}; !slices.Equal(plan.extraRepos[1].Branches, want) {
			t.Errorf("extra /src/y branches = %v, want %v", plan.extraRepos[1].Branches, want)
		}
		if base := preflight.extraBases[1][1]; base.upstreamRemote != "." || base.upstreamBranch != "support" {
			t.Errorf("extra y topic tracking = %q/%q, want ./support", base.upstreamRemote, base.upstreamBranch)
		}
		if !slices.Contains(preflight.extraRefspecs[1], "refs/heads/support:refs/heads/support") {
			t.Errorf("extra y refspecs = %v, want local upstream support mirrored", preflight.extraRefspecs[1])
		}
	})

	errCases := []struct {
		name string
		spec []ForkRepo
	}{
		{"missing_source_repo", []ForkRepo{{GitRoot: "/src/a", SourceBranches: []string{"main", "feat"}, DestPrimary: "a0"}}}, // /src/b missing.
		{"source_branch_mismatch", []ForkRepo{
			{GitRoot: "/src/a", SourceBranches: []string{"other"}, DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		{"empty_dest", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: ""},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		{"duplicate_gitroot", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/a", DestPrimary: "a1"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
		// Reusing a mapped branch would map one host branch into two containers,
		// and Fork force-resets the destination host branch.
		{"dest_reuses_source_primary", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "main"},
		}},
		{"dest_reuses_source_non_primary_branch", []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "feat"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
		}},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := planFork(t.Context(), testLogger(t), "", source, tc.spec); err == nil {
				t.Fatal("planFork error = nil, want error")
			}
		})
	}

	// An added repo resolves its own branches, so its destination is checked
	// against those rather than against a source container's mapping.
	t.Run("error_added_repo_dest_reuses_its_own_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		remote := filepath.Join(t.TempDir(), "z.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", remote)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "branch", "topic")
		runTestGit(t, ctx, dir, "remote", "add", "origin", remote)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main", "topic")
		spec := []ForkRepo{
			{GitRoot: "/src/a", DestPrimary: "a0"},
			{GitRoot: "/src/b", DestPrimary: "b0"},
			{GitRoot: dir, SourceBranches: []string{"main", "topic"}, DestPrimary: "topic"},
		}
		if _, err := planFork(ctx, testLogger(t), "", source, spec); err == nil {
			t.Fatal("planFork error = nil, want error")
		}
	})
}

func TestBackupContainerBranchesCommands(t *testing.T) {
	t.Parallel()
	// newRepo returns a repository whose detached HEAD is not an ancestor of
	// the incoming destination, so resetting the mapped branch would lose it.
	newRepo := func(t *testing.T) (string, containerBranchBase) {
		dir := t.TempDir()
		ctx := t.Context()
		runTestGit(t, ctx, dir, "init", "-q", "-b", "main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")
		runTestGit(t, ctx, dir, "update-ref", containerIncomingRefPrefix+"feature", "HEAD")
		runTestGit(t, ctx, dir, "checkout", "-q", "--detach")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "detached work")
		runTestGit(t, ctx, dir, "branch", "-q", "-D", "main")
		return dir, containerBranchBase{branch: "feature", destination: containerIncomingRefPrefix + "feature"}
	}
	run := func(t *testing.T, dir string, commands []string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // generated command is the value under test.
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("missing branch is skipped", func(t *testing.T) {
			t.Parallel()
			dir, base := newRepo(t)
			out, err := run(t, dir, backupContainerBranchesCommands([]containerBranchBase{base}, "backup-1", true))
			if err != nil || out != "backup-1" {
				t.Fatalf("backup = %q, %v", out, err)
			}
			head := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			if got := runTestGit(t, t.Context(), dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"); got != "refs/heads/backup-1 "+head {
				t.Fatalf("branches = %q, want only the HEAD backup", got)
			}
		})
		t.Run("present branch is preserved", func(t *testing.T) {
			t.Parallel()
			dir, base := newRepo(t)
			runTestGit(t, t.Context(), dir, "branch", "-q", "feature")
			out, err := run(t, dir, backupContainerBranchesCommands([]containerBranchBase{base}, "backup-1", true))
			if err != nil || out != "backup-1" {
				t.Fatalf("backup = %q, %v", out, err)
			}
			head := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			if got := runTestGit(t, t.Context(), dir, "rev-parse", "refs/heads/backup-1-0-feature"); got != head {
				t.Fatalf("branch backup = %q, want %q", got, head)
			}
		})
		t.Run("contained history needs no backup", func(t *testing.T) {
			t.Parallel()
			dir, base := newRepo(t)
			runTestGit(t, t.Context(), dir, "update-ref", base.destination, "HEAD")
			out, err := run(t, dir, backupContainerBranchesCommands([]containerBranchBase{base}, "backup-1", true))
			if err != nil || out != "" {
				t.Fatalf("backup = %q, %v", out, err)
			}
			if got := runTestGit(t, t.Context(), dir, "for-each-ref", "refs/heads/"); got != "" {
				t.Fatalf("branches = %q, want none", got)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		// A mapped branch of a regular repository must exist; a missing one
		// aborts the push rather than resetting without a backup.
		dir, base := newRepo(t)
		if out, err := run(t, dir, backupContainerBranchesCommands([]containerBranchBase{base}, "backup-1", false)); err == nil {
			t.Fatalf("missing branch accepted: %q", out)
		}
		if got := runTestGit(t, t.Context(), dir, "for-each-ref", "refs/heads/"); got != "" {
			t.Fatalf("branches = %q, want none", got)
		}
	})
}

func TestSubmoduleTree(t *testing.T) {
	t.Parallel()
	leaf := &Submodule{GitlinkPath: "inner/leaf"}
	tree := SubmoduleTree{&Submodule{GitlinkPath: "lib/dep", Children: SubmoduleTree{leaf}}}
	t.Run("ParentFirst", func(t *testing.T) {
		t.Parallel()
		var got []string
		for p := range tree.ParentFirst() {
			got = append(got, p)
		}
		want := []string{"lib/dep", "lib/dep/inner/leaf"}
		if !slices.Equal(got, want) {
			t.Errorf("parent-first paths = %v, want %v", got, want)
		}
	})
	t.Run("ChildrenFirst", func(t *testing.T) {
		t.Parallel()
		var got []string
		for p := range tree.ChildrenFirst() {
			got = append(got, p)
		}
		want := []string{"lib/dep/inner/leaf", "lib/dep"}
		if !slices.Equal(got, want) {
			t.Errorf("children-first paths = %v, want %v", got, want)
		}
	})
	t.Run("Find", func(t *testing.T) {
		t.Parallel()
		if got, ok := tree.Find("lib/dep/inner/leaf"); !ok || got != leaf {
			t.Errorf("Find leaf = %v, %t, want %v, true", got, ok, leaf)
		}
		if got, ok := tree.Find("inner/leaf"); ok || got != nil {
			t.Errorf("Find unmapped path = %v, %t, want nil, false", got, ok)
		}
	})
}

func TestContainer(t *testing.T) { //nolint:tparallel // Pull uses fakeSSH with t.Setenv.
	t.Run("launch_rejects_invalid_object_copy_threshold", func(t *testing.T) {
		t.Parallel()
		c := &Container{}
		err := c.Launch(t.Context(), io.Discard, io.Discard, &StartOpts{GitObjectCopyMinBytes: -2})
		if err == nil || !strings.Contains(err.Error(), "GitObjectCopyMinBytes must be -1 or nonnegative") {
			t.Fatalf("Launch with invalid Git object copy threshold = %v", err)
		}
	})
	t.Run("prepareTailscaleAuthKey", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			opts := StartOpts{}
			if err := prepareTailscaleAuthKey(t.Context(), &opts); err != nil || opts.tailscaleAuthKey != "" {
				t.Fatalf("prepareTailscaleAuthKey without Tailscale = %q, %v", opts.tailscaleAuthKey, err)
			}
		})
	})
	t.Run("configure_global_git_identity_uses_host_global_config", func(t *testing.T) {
		ctx := t.Context()
		fakeSSH(t)
		sshLogPath := filepath.Join(t.TempDir(), "ssh.log")
		t.Setenv(fakeSSHLogEnv, sshLogPath)
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		config := filepath.Join(t.TempDir(), "gitconfig")
		writeTestFile(t, config, "[user]\n\tname = Global User\n\temail = global@example.com\n")

		logger := testLogger(t)
		ct := &Container{
			Client: &Client{
				Home:          home,
				XDGConfigHome: filepath.Join(home, ".config"),
				Logger:        logger,
				Runtime:       testRunningRuntime(t, logger),
				env:           []string{"GIT_CONFIG_GLOBAL=" + config, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0"},
			},
			Logger: logger,
			Name:   "md-test",
		}
		if err := ct.configureGlobalGitIdentity(ctx, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		sshLog, err := os.ReadFile(sshLogPath) //nolint:gosec // test log is under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"git config --global --replace-all user.name 'Global User'",
			"git config --global --replace-all user.email global@example.com",
		} {
			if !strings.Contains(string(sshLog), want) {
				t.Errorf("global identity command = %q, want %q", sshLog, want)
			}
		}
	})
	t.Run("configure_container_remotes_propagates_git_identity", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		fakeSSH(t)
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		hostDir := t.TempDir()
		containerDir := t.TempDir()
		runTestGit(t, ctx, hostDir, "init", "-q")
		runTestGit(t, ctx, hostDir, "config", "user.name", "Marc-Antoine Ruel")
		runTestGit(t, ctx, hostDir, "config", "user.email", "maruel@example.com")
		runTestGit(t, ctx, hostDir, "config", "core.hooksPath", "scripts/hooks")
		runTestGit(t, ctx, containerDir, "init", "-q")

		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       hostDir,
				ContainerPath: filepath.ToSlash(containerDir),
			}},
		}
		if err := ct.configureContainerRemotes(ctx, io.Discard, io.Discard, &ct.Repos[0], true); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "user.name"); got != "Marc-Antoine Ruel" {
			t.Errorf("container user.name = %q, want Marc-Antoine Ruel", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "user.email"); got != "maruel@example.com" {
			t.Errorf("container user.email = %q, want maruel@example.com", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--local", "core.hooksPath"); got != "scripts/hooks" {
			t.Errorf("container core.hooksPath = %q, want scripts/hooks", got)
		}
	})
	t.Run("launch_rejects_extra_branch_tracking_primary_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		hostDir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, hostDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, hostDir, "remote", "set-head", "origin", "main")
		runTestGit(t, ctx, hostDir, "branch", "feature")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.merge", "refs/heads/main")

		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:  hostDir,
				Branches: []string{"main", "feature"},
			}},
		}
		err = ct.Launch(ctx, io.Discard, io.Discard, &StartOpts{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
			t.Fatalf("Launch error = %v, want local primary-upstream error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		if len(logData) != 0 {
			t.Fatalf("Launch invoked runtime before branch validation:\n%s", logData)
		}
	})
	t.Run("operations_reject_extra_branch_tracking_primary", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, _ := setupMultiBranchPullTest(t)
		runTestGit(t, ctx, hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.feature.merge", "refs/heads/main")

		for _, op := range []struct {
			name string
			run  func() error
		}{
			{name: "Diff", run: func() error { return ct.Diff(ctx, io.Discard, io.Discard, RepoTarget{}, nil) }},
			{name: "Pull", run: func() error { return ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil) }},
			{name: "Push", run: func() error {
				_, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{})
				return err
			}},
		} {
			err := op.run()
			if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
				t.Errorf("%s error = %v, want local primary-upstream error", op.name, err)
			}
		}
	})
	t.Run("diff_rejects_host_branch_without_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "branch", "--unset-upstream", "main")

		err := ct.Diff(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "main" has no upstream`) || !strings.Contains(err.Error(), "git -C "+shellQuote(hostDir)+" branch --set-upstream-to=origin/main main") {
			t.Fatalf("Diff error = %v, want missing host upstream repair command", err)
		}
	})
	t.Run("diff_resolves_defaults_without_altering_mapping", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		ct, _, _ := setupPullTest(t)
		ct.Repos[0].DefaultBranch = ""
		configured := ct.Repos[0]
		if err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ct.Repos[0], configured) {
			t.Fatalf("configured mapping changed: got %+v, want %+v", ct.Repos[0], configured)
		}
	})
	t.Run("diff_exit_status_flags_report_differences", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		for _, test := range []struct {
			name       string
			args       []string
			dirty      bool
			wantErr    bool
			wantOutput string
		}{
			{name: "exit_code", args: []string{"--exit-code", "--name-only"}, dirty: true, wantErr: true, wantOutput: "shared.txt"},
			{name: "quiet", args: []string{"--quiet"}, dirty: true, wantErr: true},
			{name: "no_differences", args: []string{"--exit-code"}},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, hostDir, containerDir := setupPullTest(t)
				if test.dirty {
					writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "different\n")
				} else {
					runTestGit(t, t.Context(), hostDir, "push", "-q", "origin", "main")
				}
				var stdout, stderr bytes.Buffer
				err := ct.Diff(t.Context(), &stdout, &stderr, RepoTarget{}, &DiffOpts{Args: test.args})
				if got := errors.Is(err, ErrDiffFound); got != test.wantErr {
					t.Fatalf("Diff error is ErrDiffFound = %v, want %v: %v", got, test.wantErr, err)
				} else if !test.wantErr && err != nil {
					t.Fatalf("Diff error = %v, want nil", err)
				}
				if got := strings.TrimSpace(stdout.String()); got != test.wantOutput {
					t.Errorf("Diff output = %q, want %q", got, test.wantOutput)
				}
				if stderr.Len() != 0 {
					t.Errorf("Diff stderr = %q, want empty", stderr.String())
				}
			})
		}
	})
	t.Run("diff_forwards_base_notes_with_exit_status_flags", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		ctx := t.Context()
		ct, _, containerDir := setupPullTest(t)
		runTestGit(t, ctx, containerDir, "update-ref", "-d", containerSyncRefPrefix+"main")
		writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "different\n")

		var stdout, stderr bytes.Buffer
		err := ct.Diff(ctx, &stdout, &stderr, RepoTarget{}, &DiffOpts{Args: []string{"--exit-code", "--name-only"}})
		if !errors.Is(err, ErrDiffFound) {
			t.Fatalf("Diff error = %v, want ErrDiffFound", err)
		}
		if !strings.Contains(stderr.String(), "has no recorded host integration point") {
			t.Errorf("Diff stderr = %q, want the missing sync point note", stderr.String())
		}
	})
	t.Run("host_upstream_repair_command_targets_mapped_repo", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, t.Context(), hostDir, "branch", "--unset-upstream", "main")

		_, _, err := ct.Repos[0].requiredMappedBranchUpstream(t.Context(), ct.Logger, "main")
		if err == nil {
			t.Fatal("requiredMappedBranchUpstream error = nil, want repair guidance")
		}
		_, command, ok := strings.Cut(err.Error(), "run: ")
		if !ok {
			t.Fatalf("repair command missing from error: %v", err)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-c", command) //nolint:gosec // generated command is the value under test.
		cmd.Dir = t.TempDir()
		if out, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("repair command failed outside mapped repo: %v\n%s", runErr, out)
		}
		if got := runTestGit(t, t.Context(), hostDir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "main@{upstream}"); got != "origin/main" {
			t.Fatalf("repaired upstream = %q, want origin/main", got)
		}
	})
	t.Run("host_upstream_repair_requires_usable_default", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name        string
			configure   func(*testing.T, string)
			wantProblem string
		}{
			{
				name:        "recorded_remote_missing",
				configure:   func(*testing.T, string) {},
				wantProblem: "no git remotes configured",
			},
			{
				name: "remote_tracking_ref_missing",
				configure: func(t *testing.T, dir string) {
					runTestGit(t, t.Context(), dir, "remote", "add", "origin", "/dev/null")
				},
				wantProblem: "does not exist",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
				runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "main")
				test.configure(t, dir)
				repo := Repo{
					GitRoot:       dir,
					Branches:      []string{"main"},
					Remotes:       []string{"origin"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}

				_, _, err := repo.requiredMappedBranchUpstream(t.Context(), testLogger(t), "main")
				if err == nil || !strings.Contains(err.Error(), test.wantProblem) || !strings.Contains(err.Error(), "choose a remote branch") {
					t.Fatalf("requiredMappedBranchUpstream error = %v, want %q and explicit choice", err, test.wantProblem)
				}
				if strings.Contains(err.Error(), "run:") {
					t.Fatalf("error suggests an unusable command: %v", err)
				}
			})
		}
	})
	t.Run("diff_guides_mapped_host_branch_repairs", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		for _, test := range []struct {
			name        string
			setup       func(*testing.T, *Container, string, string)
			wantProblem string
			commandArgs []string
		}{
			{
				name: "deleted_branch",
				setup: func(t *testing.T, _ *Container, hostDir, _ string) {
					runTestGit(t, t.Context(), hostDir, "switch", "-q", "--detach")
					runTestGit(t, t.Context(), hostDir, "branch", "-D", "main")
				},
				wantProblem: `mapped host branch "main" no longer exists`,
				commandArgs: []string{"branch", "--track", "main", "origin/main"},
			},
			{
				name: "incomplete_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					ct.Repos[0].Remotes = nil
					ct.Repos[0].DefaultRemote = ""
					ct.Repos[0].DefaultBranch = ""
					runTestGit(t, t.Context(), hostDir, "config", "--unset", "branch.main.merge")
				},
				wantProblem: `incomplete upstream configuration for branch "main"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "invalid_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					ct.Repos[0].Remotes = nil
					ct.Repos[0].DefaultRemote = ""
					ct.Repos[0].DefaultBranch = ""
					runTestGit(t, t.Context(), hostDir, "config", "branch.main.merge", "refs/tags/main")
				},
				wantProblem: `invalid upstream ref for branch "main": "refs/tags/main"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "md_transport_upstream",
				setup: func(t *testing.T, ct *Container, hostDir, _ string) {
					runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
					runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)
				},
				wantProblem: `mapped host branch "main" tracks md container remote "md-test"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
			{
				name: "missing_local_upstream",
				setup: func(t *testing.T, _ *Container, hostDir, containerDir string) {
					for _, dir := range []string{hostDir, containerDir} {
						runTestGit(t, t.Context(), dir, "config", "branch.main.remote", ".")
						runTestGit(t, t.Context(), dir, "config", "branch.main.merge", "refs/heads/missing")
					}
				},
				wantProblem: `mapped host branch "main" tracks missing local branch "missing"`,
				commandArgs: []string{"branch", "--set-upstream-to=origin/main", "main"},
			},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, hostDir, containerDir := setupPullTest(t)
				test.setup(t, ct, hostDir, containerDir)
				err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
				wantCommand := shellQuoteArgs(append([]string{"git", "-C", hostDir}, test.commandArgs...))
				if err == nil || !strings.Contains(err.Error(), test.wantProblem) || !strings.Contains(err.Error(), wantCommand) {
					t.Fatalf("Diff error = %v, want problem %q and command %q", err, test.wantProblem, wantCommand)
				}
			})
		}
	})
	t.Run("diff_guides_secondary_branch_away_from_primary", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupMultiBranchPullTest(t)
		runTestGit(t, t.Context(), hostDir, "config", "branch.feature.remote", ".")
		runTestGit(t, t.Context(), hostDir, "config", "branch.feature.merge", "refs/heads/main")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		wantCommand := shellQuoteArgs([]string{"git", "-C", hostDir, "branch", "--set-upstream-to=origin/main", "feature"})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) || !strings.Contains(err.Error(), wantCommand) {
			t.Fatalf("Diff error = %v, want local-primary repair command", err)
		}
	})
	t.Run("diff_resolves_legacy_defaults_before_host_upstream_validation", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		ct.Repos[0].Remotes = nil
		ct.Repos[0].DefaultRemote = ""
		ct.Repos[0].DefaultBranch = ""
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
		runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		wantCommand := shellQuoteArgs([]string{"git", "-C", hostDir, "branch", "--set-upstream-to=origin/main", "main"})
		if err == nil || !strings.Contains(err.Error(), wantCommand) {
			t.Fatalf("Diff error = %v, want resolved legacy default repair command", err)
		}
		if strings.Contains(err.Error(), "--set-upstream-to=/") {
			t.Fatalf("Diff error contains malformed repair command: %v", err)
		}
	})
	t.Run("diff_preserves_ambiguous_legacy_default_diagnostic", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, t.Context(), hostDir, "remote", "rename", "origin", "first")
		runTestGit(t, t.Context(), hostDir, "remote", "add", "second", "/dev/null")
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", "md-test")
		runTestGit(t, t.Context(), hostDir, "remote", "set-url", "md-test", "user@md-test:"+ct.Repos[0].ContainerPath)
		ct.Repos[0].Remotes = nil
		ct.Repos[0].DefaultRemote = ""
		ct.Repos[0].DefaultBranch = ""

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `multiple remotes and no "origin"`) {
			t.Fatalf("Diff error = %v, want ambiguous default diagnostic", err)
		}
		if strings.Contains(err.Error(), "--set-upstream-to=/") {
			t.Fatalf("Diff error contains malformed repair command: %v", err)
		}
	})
	t.Run("diff_reports_stopped_container", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=exited",
		}
		ct := &Container{
			Client: &Client{Home: t.TempDir(), Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{Branches: []string{"main"}}},
		}
		err = ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || err.Error() != "Container md-test is stopped. Restart it with: md start" {
			t.Fatalf("Diff error = %v, want stopped-container guidance", err)
		}
	})
	t.Run("container_state_policy", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name      string
			state     string
			policy    containerStatePolicy
			wantError string
		}{
			{name: "running_requires_running", state: "running", policy: containerMustBeRunning},
			{name: "running_allows_stopped", state: "running", policy: containerMayBeStopped},
			{name: "exited_requires_running", state: "exited", policy: containerMustBeRunning, wantError: "Container md-test is stopped. Restart it with: md start"},
			{name: "exited_allows_stopped", state: "exited", policy: containerMayBeStopped},
			{name: "paused_requires_running", state: "paused", policy: containerMustBeRunning, wantError: "Container md-test is stopped. Restart it with: md start"},
			{name: "paused_rejects_stopped", state: "paused", policy: containerMayBeStopped, wantError: "Container md-test is paused. Fork requires a running or stopped container"},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				writeTestSSHConfig(t, home)
				logger := testLogger(t)
				env := []string{
					fakeRuntimeEnv + "=1",
					fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
					fakeRuntimeStateEnv + "=" + test.state,
				}
				ct := &Container{
					Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
					Logger: logger,
					Name:   "md-test",
					State:  "running",
				}
				err := ct.checkContainerState(t.Context(), test.policy)
				if test.wantError == "" && err != nil {
					t.Fatalf("checkContainerState() = %v, want nil", err)
				}
				if test.wantError != "" && (err == nil || err.Error() != test.wantError) {
					t.Fatalf("checkContainerState() = %v, want %q", err, test.wantError)
				}
			})
		}
	})
	t.Run("container_state_surfaces_inspect_parse_error", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=raw:not-json",
		}
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			State:  "running",
		}
		err = ct.checkContainerState(t.Context(), containerMustBeRunning)
		if err == nil || !strings.Contains(err.Error(), `inspecting container "md-test"`) || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("checkContainerState() = %v, want inspect parse error", err)
		}
	})
	t.Run("container_state_checks_every_repo_remote", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		firstRepo := t.TempDir()
		secondRepo := t.TempDir()
		for _, dir := range []string{firstRepo, secondRepo} {
			runTestGit(t, t.Context(), dir, "init", "-q")
		}
		runTestGit(t, t.Context(), firstRepo, "remote", "add", "md-test", "/dev/null")
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + "=running",
		}
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{GitRoot: firstRepo}, {GitRoot: secondRepo}},
		}
		err = ct.checkContainerState(t.Context(), containerMustBeRunning)
		want := fmt.Sprintf("Git remote is missing from %q", secondRepo)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("checkContainerState() = %v, want %q", err, want)
		}
	})
	t.Run("diff_preserves_runtime_inspection_failure", func(t *testing.T) {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeErrorEnv + "=permission denied connecting to runtime socket",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		ct := &Container{
			Client: &Client{Home: t.TempDir(), Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos:  []Repo{{Branches: []string{"main"}}},
		}
		err = ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `inspecting container "md-test"`) || !strings.Contains(err.Error(), "permission denied connecting to runtime socket") {
			t.Fatalf("Diff error = %v, want runtime inspection diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("Diff error = %v, want wrapped runtime exit status 1", err)
		}
	})
	t.Run("diff_preserves_upstream_ssh_failure", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "branch.main.remote")
		t.Setenv(fakeSSHFailureTextEnv, "ssh: connect to host md-test port 22: Connection refused")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), "Connection refused") || strings.Contains(err.Error(), "has no configured upstream") {
			t.Fatalf("Diff error = %v, want classified SSH failure", err)
		}
	})
	t.Run("diff_preserves_upstream_path_failure", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, _, _ := setupPullTest(t)
		missingPath := filepath.ToSlash(filepath.Join(t.TempDir(), "missing"))
		ct.Repos[0].ContainerPath = missingPath

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), missingPath) || strings.Contains(err.Error(), "has no configured upstream") {
			t.Fatalf("Diff error = %v, want classified container path failure", err)
		}
	})
	t.Run("diff_repairs_missing_and_malformed_container_upstreams", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		for _, test := range []struct {
			name  string
			setup func(*testing.T, string)
		}{
			{name: "missing", setup: func(t *testing.T, dir string) {
				runTestGit(t, t.Context(), dir, "config", "--unset", "branch.main.remote")
			}},
			{name: "malformed", setup: func(t *testing.T, dir string) {
				runTestGit(t, t.Context(), dir, "config", "branch.main.merge", "refs/tags/main")
			}},
		} {
			t.Run(test.name, func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
				ct, _, containerDir := setupPullTest(t)
				test.setup(t, containerDir)
				err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
				if err == nil || !strings.Contains(err.Error(), "run md pull or md push") {
					t.Fatalf("Diff error = %v, want upstream repair guidance", err)
				}
			})
		}
	})
	t.Run("diff_preserves_final_ssh_failure", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureTextEnv, "ssh: connection reset by peer")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `running diff in container "md-test" over SSH`) || !strings.Contains(err.Error(), "connection reset by peer") {
			t.Fatalf("Diff error = %v, want final SSH diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 255 {
			t.Fatalf("Diff error = %v, want wrapped SSH exit status 255", err)
		}
	})
	t.Run("diff_preserves_final_ssh_exit_one_diagnostic", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureStatusEnv, "1")
		t.Setenv(fakeSSHFailureTextEnv, "fatal: simulated remote Git failure")

		err := ct.Diff(t.Context(), io.Discard, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--exit-code"}})
		if err == nil || errors.Is(err, ErrDiffFound) || !strings.Contains(err.Error(), `running diff in container "md-test" over SSH`) || !strings.Contains(err.Error(), "fatal: simulated remote Git failure") {
			t.Fatalf("Diff error = %v, want wrapped remote Git diagnostic", err)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("Diff error = %v, want wrapped SSH exit status 1", err)
		}
	})
	t.Run("diff_forwards_stderr_when_differences_are_found", func(t *testing.T) {
		ct, _, _ := setupPullTest(t)
		t.Setenv(fakeSSHFailureMatchEnv, "GIT_OPTIONAL_LOCKS=0")
		t.Setenv(fakeSSHFailureStatusEnv, strconv.Itoa(diffFoundSSHExitCode))
		t.Setenv(fakeSSHFailureTextEnv, "warning: simulated Git warning")
		var stderr bytes.Buffer

		err := ct.Diff(t.Context(), io.Discard, &stderr, RepoTarget{}, &DiffOpts{Args: []string{"--quiet"}})
		if !errors.Is(err, ErrDiffFound) {
			t.Fatalf("Diff error = %v, want ErrDiffFound", err)
		}
		if got := strings.TrimSpace(stderr.String()); got != "warning: simulated Git warning" {
			t.Fatalf("Diff stderr = %q, want forwarded Git warning", got)
		}
	})
	t.Run("diff_rejects_changed_host_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "push", "-q", "origin", "main:release")
		runTestGit(t, ctx, hostDir, "fetch", "-q", "origin", "release")
		runTestGit(t, ctx, hostDir, "branch", "--set-upstream-to=origin/release", "main")

		err := ct.Diff(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `upstream changed from "origin/main" in the container to "origin/release" on the host`) || !strings.Contains(err.Error(), "run md pull or md push") {
			t.Fatalf("Diff error = %v, want changed upstream guidance", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.merge"); got != "refs/heads/main" {
			t.Fatalf("container branch merge ref after Diff = %q, want unchanged refs/heads/main", got)
		}
		if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
			t.Fatalf("Pull after upstream change: %v", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.remote"); got != "origin" {
			t.Errorf("container branch remote = %q, want origin", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.merge"); got != "refs/heads/release" {
			t.Errorf("container branch merge ref = %q, want refs/heads/release", got)
		}
	})
	t.Run("diff_rejects_changed_extra_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupMultiBranchPullTest(t)
		runTestGit(t, ctx, containerDir, "switch", "-q", "-c", "feature", "origin/feature")
		runTestGit(t, ctx, hostDir, "push", "-q", "origin", "feature:release")
		runTestGit(t, ctx, hostDir, "fetch", "-q", "origin", "release")
		runTestGit(t, ctx, hostDir, "branch", "--set-upstream-to=origin/release", "feature")

		err := ct.Diff(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
		if err == nil || !strings.Contains(err.Error(), `mapped branch "feature" upstream changed`) || !strings.Contains(err.Error(), "run md pull or md push") {
			t.Fatalf("Diff error = %v, want changed extra-branch upstream guidance", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.feature.merge"); got != "refs/heads/feature" {
			t.Fatalf("container feature merge ref after Diff = %q, want unchanged refs/heads/feature", got)
		}
	})
	t.Run("revive_recovers_missing_local_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "branch", "agy", "origin/main")
		runTestGit(t, ctx, hostDir, "config", "branch.main.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.main.merge", "refs/heads/agy")
		ct.Repos[0].DefaultBranch = "agy"
		if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
			t.Fatal(err)
		}
		want := runTestGit(t, ctx, containerDir, "rev-parse", "agy")
		runTestGit(t, ctx, hostDir, "branch", "-D", "agy")
		if err := ct.restoreLocalUpstreams(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, hostDir, "rev-parse", "agy"); got != want {
			t.Fatalf("restored agy = %q, want %q", got, want)
		}
		if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
			t.Fatalf("sync after recovery: %v", err)
		}
		// A retry must not overwrite a branch that now exists on the host.
		runTestGit(t, ctx, hostDir, "update-ref", "refs/heads/agy", "main")
		want = runTestGit(t, ctx, hostDir, "rev-parse", "agy")
		if err := ct.restoreLocalUpstreams(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, hostDir, "rev-parse", "agy"); got != want {
			t.Fatalf("recovery moved existing agy to %q, want %q", got, want)
		}
	})
	t.Run("revive_missing_local_upstream_absent_in_container", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ct, hostDir, _ := setupPullTest(t)
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.remote", ".")
		runTestGit(t, t.Context(), hostDir, "config", "branch.main.merge", "refs/heads/agy")
		if err := ct.restoreLocalUpstreams(t.Context(), 0); err == nil || !strings.Contains(err.Error(), "agy") {
			t.Fatalf("recovery error = %v, want missing agy error", err)
		}
		g := &git.Checkout{Root: hostDir, Logger: ct.Logger}
		if exists, err := g.RefExists(t.Context(), "refs/heads/agy"); err != nil || exists {
			t.Fatalf("agy exists = %v, err = %v, want absent", exists, err)
		}
	})
	t.Run("diff_syncs_local_branch_upstream", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		runTestGit(t, ctx, hostDir, "branch", "migration", "origin/main")
		runTestGit(t, ctx, hostDir, "config", "branch.main.remote", ".")
		runTestGit(t, ctx, hostDir, "config", "branch.main.merge", "refs/heads/migration")

		if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
			t.Fatalf("Pull after local upstream change: %v", err)
		}
		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}, Full: true}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "shared.txt" {
			t.Fatalf("full diff --name-only = %q, want shared.txt", got)
		}
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "" {
			t.Fatalf("diff --name-only after pull = %q, want no change since the sync point", got)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "migration"); got != runTestGit(t, ctx, hostDir, "rev-parse", "migration") {
			t.Errorf("container migration = %q, want host migration", got)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.remote"); got != "." {
			t.Errorf("container branch remote = %q, want .", got)
		}
	})
	t.Run("diff_reports_container_work_since_the_last_pull", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, _, containerDir := setupPullTest(t)
		syncPoint := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
		if want := runTestGit(t, ctx, containerDir, "rev-parse", "main"); syncPoint != want {
			t.Fatalf("sync point after pull = %q, want container main %q", syncPoint, want)
		}
		writeTestFile(t, filepath.Join(containerDir, "turn.txt"), "turn\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "turn")

		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "turn.txt" {
			t.Fatalf("diff --name-only = %q, want turn.txt", got)
		}
		// Amending the branch tip must not move the base of the diff.
		writeTestFile(t, filepath.Join(containerDir, "amended.txt"), "amended\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "--no-edit")
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "amended.txt\nturn.txt"; got != want {
			t.Fatalf("diff --name-only after amend = %q, want %q", got, want)
		}
		stdout.Reset()
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}, Full: true}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "amended.txt\nshared.txt\nturn.txt"; got != want {
			t.Fatalf("full diff --name-only = %q, want %q", got, want)
		}
	})
	t.Run("mapped_submodule_sync", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, source, _ := setupPullTest(t)
		host, dst := canonicalTempDir(t), canonicalTempDir(t)
		for _, root := range []string{host, dst} {
			runTestGit(t, ctx, root, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", "different-name", source, "lib/dep")
		}
		runTestGit(t, ctx, host, "remote", "add", ct.Name, "user@"+ct.Name+":"+filepath.ToSlash(dst))
		hm, cm := filepath.Join(host, "lib", "dep"), filepath.Join(dst, "lib", "dep")
		runTestGit(t, ctx, hm, "config", "user.name", "Test")
		runTestGit(t, ctx, hm, "config", "user.email", "test@test")
		runTestGit(t, ctx, cm, "update-ref", containerSubmoduleHeadRef, "HEAD")
		runTestGit(t, ctx, cm, "update-ref", containerSyncRefPrefix+"HEAD", "HEAD")
		runTestGit(t, ctx, cm, "update-ref", containerSyncRefPrefix+"main", "main")
		runTestGit(t, ctx, cm, "checkout", "-q", "--detach")
		ct.Repos = []Repo{{GitRoot: host, ContainerPath: filepath.ToSlash(dst), Branches: []string{"main"}}}
		configured := slices.Clone(ct.Repos)
		assertMappings := func() {
			if !reflect.DeepEqual(ct.Repos, configured) {
				t.Fatalf("configured mappings changed: got %+v, want %+v", ct.Repos, configured)
			}
		}
		runTestGit(t, ctx, "", "init", "-q", filepath.Join(host, "standalone"))
		for _, test := range []struct {
			target RepoTarget
			err    string
		}{
			{RepoTarget{Branch: "main"}, "branch override requires a submodule"},
			{RepoTarget{Submodule: "../dep"}, "invalid submodule path"},
			{RepoTarget{Submodule: "lib/../lib/dep"}, "invalid submodule path"},
			{RepoTarget{Submodule: "lib"}, "not an initialized submodule worktree"},
			{RepoTarget{Submodule: "standalone"}, "is not a submodule"},
		} {
			if err := ct.Diff(ctx, io.Discard, io.Discard, test.target, nil); err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("Diff(%+v) = %v, want %q", test.target, err, test.err)
			}
			assertMappings()
		}
		target := RepoTarget{Submodule: "lib/dep"}
		writeTestFile(t, filepath.Join(cm, "work.txt"), "container work\n")
		var diff bytes.Buffer
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(diff.String(), "work.txt") {
			t.Fatalf("missing submodule diff: %s", diff.String())
		}
		if got := runTestGit(t, ctx, cm, "branch", "--show-current"); got != "" {
			t.Fatalf("diff attached detached HEAD to %q", got)
		}
		diff.Reset()
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}, Full: true}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(diff.String(), "work.txt") {
			t.Fatalf("missing full submodule diff: %s", diff.String())
		}

		if err := ct.Pull(ctx, io.Discard, io.Discard, target, nil); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(hm, "work.txt")); err != nil || string(got) != "container work\n" { //nolint:gosec // private test checkout.
			t.Fatalf("pulled work: %q, %v", got, err)
		}

		diff.Reset()
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if diff.Len() != 0 {
			t.Fatalf("diff after pulling submodule: %s", diff.String())
		}
		writeTestFile(t, filepath.Join(hm, "host.txt"), "host work\n")
		runTestGit(t, ctx, hm, "add", ".")
		runTestGit(t, ctx, hm, "commit", "-qm", "host work")
		if _, err := ct.Push(ctx, io.Discard, io.Discard, target); err != nil {
			t.Fatal(err)
		}
		diff.Reset()
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if diff.Len() != 0 {
			t.Fatalf("diff after push: %s", diff.String())
		}
		runTestGit(t, ctx, cm, "checkout", "-q", "--detach")
		diff.Reset()
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if diff.Len() != 0 {
			t.Fatalf("detached diff after integration: %s", diff.String())
		}
		// A new host branch can be pushed even when it was absent at startup.
		runTestGit(t, ctx, hm, "switch", "-qc", "new-work")
		runTestGit(t, ctx, hm, "branch", "--set-upstream-to=origin/main", "new-work")
		var missingBranch bytes.Buffer
		if err := ct.Pull(ctx, io.Discard, &missingBranch, target, nil); err == nil || !strings.Contains(missingBranch.String(), "run md push") {
			t.Fatalf("missing container branch pull: %v, %s", err, missingBranch.String())
		}
		if _, err := ct.Push(ctx, io.Discard, io.Discard, target); err != nil {
			t.Fatal(err)
		}
		assertMappings()
		runTestGit(t, ctx, hm, "switch", "-q", "--detach")
		if err := ct.Diff(ctx, io.Discard, io.Discard, target, nil); err == nil || !strings.Contains(err.Error(), "use -branch") {
			t.Fatalf("Diff with detached host and implicit branch = %v", err)
		}
		target.Branch = "new-work"
		if err := ct.Diff(ctx, io.Discard, io.Discard, target, nil); err != nil {
			t.Fatal(err)
		}
		assertMappings()
		runTestGit(t, ctx, cm, "update-ref", "-d", containerSyncRefPrefix+"HEAD")
		if err := ct.Diff(ctx, io.Discard, io.Discard, target, nil); err == nil || !strings.Contains(err.Error(), "submodule integration point is missing") {
			t.Fatalf("Diff without a provisioned submodule base = %v", err)
		}
		assertMappings()
		if err := ct.removeSubmoduleRemotes(ctx, &ct.Repos[0]); err != nil {
			t.Fatal(err)
		}
		remotes := runTestGit(t, ctx, hm, "remote")
		if remotes != "origin" {
			t.Fatalf("remotes after cleanup = %q", remotes)
		}
	})
	t.Run("pinned_submodule_preserves_branch_history", func(t *testing.T) {
		ctx := t.Context()
		ct, source, _ := setupPullTest(t)
		t.Setenv("GIT_ALLOW_PROTOCOL", "file:ssh")
		host, dst := canonicalTempDir(t), canonicalTempDir(t)
		runTestGit(t, ctx, host, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, host, "config", "user.name", "Test")
		runTestGit(t, ctx, host, "config", "user.email", "test@test")
		runTestGit(t, ctx, host, "submodule", "add", "-q", source, "dep")
		runTestGit(t, ctx, host, "commit", "-qm", "pin submodule")
		hm, cm := filepath.Join(host, "dep"), filepath.Join(dst, "dep")
		runTestGit(t, ctx, hm, "config", "user.name", "Test")
		runTestGit(t, ctx, hm, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(hm, "published.txt"), "published host commit\n")
		runTestGit(t, ctx, hm, "add", ".")
		runTestGit(t, ctx, hm, "commit", "-qm", "published")
		tip := runTestGit(t, ctx, hm, "rev-parse", "main")
		runTestGit(t, ctx, "", "clone", "-q", host, dst)
		if err := ct.pushSubmodules(ctx, io.Discard, io.Discard, filepath.ToSlash(dst), host, "", true); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, ctx, host, "remote", "add", ct.Name, "user@"+ct.Name+":"+filepath.ToSlash(dst))
		ct.Repos = []Repo{{GitRoot: host, ContainerPath: filepath.ToSlash(dst), Branches: []string{"main"}}}
		target := RepoTarget{Submodule: "dep"}
		var initial bytes.Buffer
		if err := ct.Diff(ctx, &initial, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if initial.Len() != 0 {
			t.Fatalf("fresh pinned submodule diff: %s", initial.String())
		}
		writeTestFile(t, filepath.Join(cm, "work.txt"), "detached work\n")
		runTestGit(t, ctx, cm, "add", ".")
		runTestGit(t, ctx, cm, "commit", "-qm", "detached work")
		var diff bytes.Buffer
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(diff.String()); got != "work.txt" {
			t.Fatalf("pinned diff = %q", got)
		}
		var stderr bytes.Buffer
		if err := ct.Pull(ctx, io.Discard, &stderr, target, nil); err == nil || !strings.Contains(stderr.String(), "not an ancestor") {
			t.Fatalf("diverged detached pull = %v", err)
		}
		for _, root := range []string{hm, cm} {
			if got := runTestGit(t, ctx, root, "rev-parse", "main"); got != tip {
				t.Fatalf("branch rewritten in %s: %s != %s", root, got, tip)
			}
		}
		runTestGit(t, ctx, cm, "switch", "-q", "main")
		diff.Reset()
		if err := ct.Diff(ctx, &diff, io.Discard, target, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if diff.Len() != 0 {
			t.Fatalf("host commits included in branch diff: %s", diff.String())
		}
	})
	t.Run("fetch_without_commit_preserves_unintegrated_diff", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
		writeTestFile(t, filepath.Join(containerDir, "committed.txt"), "committed\n")
		runTestGit(t, ctx, containerDir, "add", ".")
		runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "committed work")
		containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
		writeTestFile(t, filepath.Join(containerDir, "pending.txt"), "pending\n")

		fetched, err := ct.Fetch(ctx, io.Discard, io.Discard, 0, nil)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if want := []FetchedBranch{{BranchName: "main", CommitHash: containerTip}}; !reflect.DeepEqual(fetched, want) {
			t.Errorf("Fetch result = %+v, want %+v", fetched, want)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
			t.Errorf("container main = %q, want the tip %q it had before the fetch", got, containerTip)
		}
		if got := runTestGit(t, ctx, containerDir, "status", "--porcelain"); got != "?? pending.txt" {
			t.Errorf("container status = %q, want pending.txt still uncommitted", got)
		}
		if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != containerTip {
			t.Errorf("host tracking ref = %q, want the committed tip %q", got, containerTip)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
			t.Errorf("sync point = %q, want unchanged %q", got, previousSync)
		}
		// Fetching makes the commits durable on the host without acknowledging
		// them as integrated into its branch.
		var stdout bytes.Buffer
		if err := ct.Diff(ctx, &stdout, io.Discard, RepoTarget{}, &DiffOpts{Args: []string{"--name-only"}}); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.TrimSpace(stdout.String()), "committed.txt\npending.txt"; got != want {
			t.Errorf("diff --name-only = %q, want %q", got, want)
		}
	})
	t.Run("pull_refreshes_changed_push_remote", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		ct, hostDir, containerDir := setupPullTest(t)
		publishDir := filepath.Join(t.TempDir(), "publish.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", publishDir)
		runTestGit(t, ctx, hostDir, "remote", "add", "publish", publishDir)
		runTestGit(t, ctx, hostDir, "config", "branch.main.pushRemote", "publish")

		if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
			t.Fatalf("Pull after push remote change: %v", err)
		}
		if got := runTestGit(t, ctx, containerDir, "config", "--get", "branch.main.pushRemote"); got != "publish" {
			t.Fatalf("container branch push remote = %q, want publish", got)
		}
	})
	t.Run("command_logs_redact_sensitive_values", func(t *testing.T) {
		t.Parallel()
		rt, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		logger := slog.New(slog.NewTextHandler(io.MultiWriter(&log, testLogWriter{t: t}), testLoggerOptions()))
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		c := &Client{
			Logger:  logger,
			Runtime: testRuntime(t, rt, logger, env),
			env:     env,
		}
		args := []string{
			rt, "build",
			"-e", "TAILSCALE_AUTHKEY=tskey-secret",
			"--env=MD_SUDO_PASSWORD=sudo-secret",
			"--env", "OPENAI_API_KEY=api-key-secret",
			"--build-arg", "API_KEY_FILE=api-key-file-secret",
			"--other=API_KEY_CONFIG=api-key-config-secret",
			"--label", "md.sudo-password=label-secret",
			"TailscaleAPIKey=camel-api-secret",
			"--password", "flag-secret",
			"--label", "md.display=1",
		}
		ct := &Container{Client: c, Logger: logger}
		if _, err := ct.runCmd(t.Context(), "", args); err != nil {
			t.Fatal(err)
		}
		if err := ct.runCmdOut(t.Context(), "", args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}

		got := log.String()
		for _, secret := range []string{"tskey-secret", "sudo-secret", "api-key-secret", "api-key-file-secret", "api-key-config-secret", "label-secret", "camel-api-secret", "flag-secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("log output leaked %q:\n%s", secret, got)
			}
		}
		for _, want := range []string{"TAILSCALE_AUTHKEY=<redacted>", "MD_SUDO_PASSWORD=<redacted>", "OPENAI_API_KEY=<redacted>", "API_KEY_FILE=<redacted>", "--other=API_KEY_CONFIG=<redacted>", "md.sudo-password=<redacted>", "TailscaleAPIKey=<redacted>", "--password", "<redacted>", "md.display=1"} {
			if !strings.Contains(got, want) {
				t.Fatalf("log output = %q, want %q", got, want)
			}
		}
	})
	t.Run("objectStore_preserves_client_config_and_isolates_git_scope", func(t *testing.T) {
		t.Parallel()
		gitDir := filepath.Join(t.TempDir(), "repo.git")
		runTestGit(t, t.Context(), "", "init", "-q", "--bare", gitDir)
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{
				Logger: logger,
				env: []string{
					"GIT_DIR=/missing/git-dir",
					"GIT_WORK_TREE=/missing/work-tree",
					"LANG=invalid",
					"GIT_CONFIG_COUNT=1",
					"GIT_CONFIG_KEY_0=alias.show-env",
					`GIT_CONFIG_VALUE_0=!printf '%s|%s|%s' "$GIT_DIR" "$GIT_WORK_TREE" "$LANG"`,
				},
			},
			Logger: logger,
		}
		out, err := ct.objectStore(gitDir).RunGit(t.Context(), "show-env")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.ToSlash(gitDir) + "||C"; out != want {
			t.Fatalf("environment = %q, want %q", out, want)
		}
	})
	t.Run("pushContainerRefs_empty", func(t *testing.T) {
		t.Parallel()
		if err := (&Container{}).pushContainerRefs(t.Context(), &Repo{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("sync_default_branch_preserves_push_diagnostic", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		hostDir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		missingContainerDir := filepath.Join(t.TempDir(), "missing-container.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, hostDir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, hostDir, "remote", "add", "md-test", missingContainerDir)

		ct := &Container{
			Client: testClient(t),
			Logger: testLogger(t),
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       hostDir,
				Branches:      []string{"main"},
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		err := ct.SyncDefaultBranch(ctx, 0)
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(hostDir)) || !strings.Contains(err.Error(), `container "md-test"`) || !strings.Contains(err.Error(), "does not appear to be a git repository") {
			t.Fatalf("SyncDefaultBranch error = %v, want repository/container context and Git stderr", err)
		}
	})
	t.Run("sync_default_branch_contextualizes_source_ref_failure", func(t *testing.T) {
		t.Parallel()
		missingRepo := filepath.Join(t.TempDir(), "missing-repository")
		ct := &Container{
			Client: testClient(t),
			Logger: testLogger(t),
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       missingRepo,
				Branches:      []string{"main"},
				Remotes:       []string{"origin"},
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		err := ct.SyncDefaultBranch(t.Context(), 0)
		if err == nil || !strings.Contains(err.Error(), missingRepo) || !strings.Contains(err.Error(), `container "md-test"`) {
			t.Fatalf("SyncDefaultBranch error = %v, want source repository/container context", err)
		}
	})
	t.Run("FetchRecursive", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
		ctx := t.Context()
		c, host, root := setupMultiBranchPullTest(t)
		initRepo := func() string {
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "file.txt"), "base\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-qm", "base")
			return dir
		}
		addModule := func(parent, name, src, link string) {
			runTestGit(t, ctx, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", name, src, link)
			runTestGit(t, ctx, parent, "commit", "-qm", "add "+link)
		}
		const childPath = "lib/dep"
		const leafLink = "inner/leaf"
		const leafPath = childPath + "/" + leafLink
		leafSource := initRepo()
		childSource := initRepo()
		addModule(childSource, "named/leaf", leafSource, leafLink)
		addModule(root, "named/dep", childSource, childPath)
		runTestGit(t, ctx, root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
		child := filepath.Join(root, "lib", "dep")
		leaf := filepath.Join(child, "inner", "leaf")
		for _, dir := range []string{child, leaf} {
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			runTestGit(t, ctx, dir, "switch", "-q", "--detach")
		}

		oldChild := runTestGit(t, ctx, child, "rev-parse", "HEAD")
		oldLeaf := runTestGit(t, ctx, leaf, "rev-parse", "HEAD")
		// These versions are neither descendant HEADs nor origin refs. The only
		// reason to transfer them is that an immutable fetched parent pins them.
		leafTree := runTestGit(t, ctx, leaf, "rev-parse", "HEAD^{tree}")
		historicalLeaf := runTestGit(t, ctx, leaf, "commit-tree", leafTree, "-p", oldLeaf, "-m", "historical leaf")
		runTestGit(t, ctx, child, "update-index", "--cacheinfo", "160000,"+historicalLeaf+","+leafLink)
		childTree := runTestGit(t, ctx, child, "write-tree")
		historicalChild := runTestGit(t, ctx, child, "commit-tree", childTree, "-p", oldChild, "-m", "historical child")
		runTestGit(t, ctx, child, "reset", "-q", "HEAD", "--", leafLink)
		runTestGit(t, ctx, root, "switch", "-q", "-c", "feature", "origin/feature")
		runTestGit(t, ctx, root, "restore", "--source=main", "--staged", "--worktree", "--", ".gitmodules")
		runTestGit(t, ctx, root, "update-index", "--add", "--cacheinfo", "160000,"+historicalChild+","+childPath)
		runTestGit(t, ctx, root, "commit", "-qm", "pin original child")
		feature := runTestGit(t, ctx, root, "rev-parse", "HEAD")
		runTestGit(t, ctx, root, "switch", "-q", "main")

		tree, err := c.DiscoverSubmodules(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) != 1 || tree[0].GitlinkPath != childPath || len(tree[0].Children) != 1 || tree[0].Children[0].GitlinkPath != leafLink {
			t.Fatalf("unexpected immediate children: %#v", tree)
		}
		foundLeaf, ok := tree.Find(leafPath)
		if !ok || foundLeaf.CommitHash != oldLeaf || foundLeaf.BranchName != "" {
			t.Fatalf("discovered leaf = %+v, want detached HEAD %s", foundLeaf, oldLeaf)
		}

		writeTestFile(t, filepath.Join(leaf, "file.txt"), "changed in container\n")
		result, err := c.FetchRecursive(ctx, io.Discard, io.Discard, 0, &FetchOpts{Commit: true})
		if err != nil {
			t.Fatal(err)
		}
		newRoot := runTestGit(t, ctx, root, "rev-parse", "HEAD")
		newChild := runTestGit(t, ctx, child, "rev-parse", "HEAD")
		newLeaf := runTestGit(t, ctx, leaf, "rev-parse", "HEAD")
		if newChild == oldChild || newLeaf == oldLeaf {
			t.Fatalf("descendant commits did not roll up: child %s, leaf %s", newChild, newLeaf)
		}
		wantBranches := []FetchedBranch{{BranchName: "main", CommitHash: newRoot}, {BranchName: "feature", CommitHash: feature}}
		if !reflect.DeepEqual(result.Branches, wantBranches) {
			t.Errorf("branches = %+v, want %+v", result.Branches, wantBranches)
		}
		wantLinks := []FetchedGitlink{
			{Path: childPath, ParentCommit: newRoot, CommitHash: newChild},
			{Path: leafPath, ParentCommit: newChild, CommitHash: newLeaf},
			{Path: childPath, ParentCommit: feature, CommitHash: historicalChild},
			{Path: leafPath, ParentCommit: historicalChild, CommitHash: historicalLeaf},
		}
		if !reflect.DeepEqual(result.Gitlinks, wantLinks) {
			t.Errorf("Gitlinks = %+v, want %+v", result.Gitlinks, wantLinks)
		}
		for _, link := range wantLinks {
			sub, ok := result.Submodules.Find(link.Path)
			if !ok {
				t.Fatalf("missing fetched submodule %s", link.Path)
			}
			if got := runTestGit(t, ctx, host, "--git-dir="+sub.GitDir, "rev-parse", "refs/md/containers/md-test/submodule/pins/"+link.CommitHash); got != link.CommitHash {
				t.Errorf("pin %s = %s, want %s", link.Path, got, link.CommitHash)
			}
			if got := runTestGit(t, ctx, sub.ContainerPath, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
				t.Errorf("submodule %s switched off detached HEAD to %s", link.Path, got)
			}
		}
		// A historical replacement at the same path can point at an object that
		// is absent from the currently initialized repository.
		runTestGit(t, ctx, root, "switch", "-q", "feature")
		runTestGit(t, ctx, root, "config", "--file", ".gitmodules", "submodule.named/dep.url", childSource+"-replacement")
		runTestGit(t, ctx, root, "add", ".gitmodules")
		missing := strings.Repeat("1", len(oldChild))
		runTestGit(t, ctx, root, "update-index", "--cacheinfo", "160000,"+missing+","+childPath)
		runTestGit(t, ctx, root, "commit", "-qm", "replace child repository")
		replacement := runTestGit(t, ctx, root, "rev-parse", "HEAD")
		runTestGit(t, ctx, root, "switch", "-q", "main")
		afterReplacement, err := c.FetchRecursive(ctx, io.Discard, io.Discard, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(afterReplacement.Branches) != 2 || afterReplacement.Branches[1].CommitHash != replacement {
			t.Fatalf("replacement branch was not fetched: %+v", afterReplacement.Branches)
		}
		if !reflect.DeepEqual(afterReplacement.Gitlinks, wantLinks[:2]) {
			t.Errorf("Gitlinks with historical replacement = %+v, want %+v", afterReplacement.Gitlinks, wantLinks[:2])
		}
		if err := c.removeSubmoduleFetchRefs(ctx, host); err != nil {
			t.Fatal(err)
		}
		for p, sub := range result.Submodules.ParentFirst() {
			if got := runTestGit(t, ctx, host, "--git-dir="+sub.GitDir, "for-each-ref", "--format=%(refname)", "refs/md/containers/md-test/submodule/"); got != "" {
				t.Errorf("submodule %s retained owned refs after cleanup: %q", p, got)
			}
		}
	})
	t.Run("pushSubmodules", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			shallow   bool
			tagRegexp string
		}{{"full", false, "^v-"}, {"shallow", true, "^v-"}, {"shallow_no_tags", true, ""}} {
			t.Run(tc.name, func(t *testing.T) {
				shallow := tc.shallow
				ctx := t.Context()
				fakeSSH(t)
				t.Setenv("GIT_ALLOW_PROTOCOL", "file:ssh")
				newRepo := func(name string) string {
					dir := filepath.Join(t.TempDir(), name)
					runTestGit(t, ctx, "", "init", "-q", "--initial-branch=main", dir)
					runTestGit(t, ctx, dir, "config", "user.name", "Test")
					runTestGit(t, ctx, dir, "config", "user.email", "test@example.invalid")
					runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "root")
					writeTestFile(t, filepath.Join(dir, "tracked.txt"), name+" base\n")
					runTestGit(t, ctx, dir, "add", ".")
					runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
					runTestGit(t, ctx, dir, "tag", "v-base")
					writeTestFile(t, filepath.Join(dir, "tracked.txt"), name+" main\n")
					runTestGit(t, ctx, dir, "commit", "-q", "-am", "main")
					runTestGit(t, ctx, dir, "tag", "v-main")
					runTestGit(t, ctx, dir, "tag", "ignored")
					runTestGit(t, ctx, dir, "checkout", "-q", "-b", "feature")
					runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "feature")
					runTestGit(t, ctx, dir, "checkout", "-q", "main")
					return dir
				}
				nested := newRepo("nested")
				module := newRepo("module")
				writeTestFile(t, filepath.Join(module, ".gitmodules"), "[submodule \"nested\"]\n\tpath = nested\n\turl = https://example.invalid/nested\n")
				nestedTip := runTestGit(t, ctx, nested, "rev-parse", "main")
				runTestGit(t, ctx, module, "update-index", "--add", "--cacheinfo", "160000,"+nestedTip+",nested")
				runTestGit(t, ctx, module, "add", ".gitmodules")
				runTestGit(t, ctx, module, "commit", "-q", "-m", "nested submodule")
				host := newRepo("host")
				writeTestFile(t, filepath.Join(host, ".gitmodules"), "[submodule \"dep\"]\n\tpath = dep\n\turl = https://example.invalid/module\n")
				moduleTip := runTestGit(t, ctx, module, "rev-parse", "main")
				runTestGit(t, ctx, host, "update-index", "--add", "--cacheinfo", "160000,"+moduleTip+",dep")
				runTestGit(t, ctx, host, "add", ".gitmodules")
				runTestGit(t, ctx, host, "commit", "-q", "-m", "submodule")
				for _, repo := range []struct{ src, path, tip string }{{module, "dep", moduleTip}, {nested, "dep/modules/nested", nestedTip}} {
					// The gitlink pins an older commit than the module's main branch.
					runTestGit(t, ctx, repo.src, "commit", "-q", "--allow-empty", "-m", "next")
					dir := filepath.Join(host, ".git", "modules", filepath.FromSlash(repo.path))
					args := []string{"clone", "-q", "--bare", "--no-local"}
					if shallow {
						args = append(args, "--depth=1", "--no-single-branch")
					}
					runTestGit(t, ctx, "", append(args, repo.src, dir)...)
					// Give the tag a distinct shallow root, independent of branch tips.
					args = []string{"--git-dir=" + dir, "fetch", "-q"}
					if shallow {
						args = append(args, "--depth=1")
					}
					runTestGit(t, ctx, host, append(args, "origin", "refs/tags/v-base:refs/tags/v-base")...)
					// Keep the gitlink commit only in detached HEAD, as submodule update does.
					args = []string{"--git-dir=" + dir, "fetch", "-q"}
					if shallow {
						args = append(args, "--depth=1")
					}
					runTestGit(t, ctx, host, append(args, "origin", repo.tip)...)
					runTestGit(t, ctx, host, "--git-dir="+dir, "update-ref", "--no-deref", "HEAD", repo.tip)
					runTestGit(t, ctx, host, "--git-dir="+dir, "config", "core.bare", "false")
					runTestGit(t, ctx, host, "--git-dir="+dir, "config", "core.worktree", filepath.Join(t.TempDir(), "missing"))
					runTestGit(t, ctx, host, "--git-dir="+dir, "config", "branch.main.remote", "origin")
					runTestGit(t, ctx, host, "--git-dir="+dir, "config", "branch.main.merge", "refs/heads/main")
					runTestGit(t, ctx, host, "--git-dir="+dir, "config", "remote.origin.pushurl", "git@example.invalid:owner/publish.git")
					runTestGit(t, ctx, host, "--git-dir="+dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main")
					runTestGit(t, ctx, host, "--git-dir="+dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
				}
				dst := filepath.Join(t.TempDir(), "container")
				runTestGit(t, ctx, "", "clone", "-q", "--no-local", host, dst)
				ct := &Container{Client: testClient(t), Logger: testLogger(t), Name: "md-test"}
				var stderr bytes.Buffer

				if !t.Run("valid", func(t *testing.T) {
					ctx := t.Context()
					if err := ct.pushSubmodules(ctx, io.Discard, &stderr, filepath.ToSlash(dst), host, tc.tagRegexp, true); err != nil {
						t.Fatalf("pushSubmodules: %v: %s", err, stderr.String())
					}
					for _, repo := range []struct{ src, path, tip string }{{module, "dep", moduleTip}, {nested, "dep/nested", nestedTip}} {
						dir := filepath.Join(dst, filepath.FromSlash(repo.path))
						if got, want := runTestGit(t, ctx, dir, "rev-parse", "HEAD"), repo.tip; got != want {
							t.Fatalf("%s HEAD = %s, want %s: %s", repo.path, got, want, stderr.String())
						}
						if got := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"HEAD"); got != repo.tip {
							t.Fatalf("%s detached baseline = %s, want %s", repo.path, got, repo.tip)
						}
						if got, want := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"main"), runTestGit(t, ctx, repo.src, "rev-parse", "main"); got != want {
							t.Fatalf("%s branch baseline = %s, want %s", repo.path, got, want)
						}
						if got := runTestGit(t, ctx, dir, "remote", "get-url", "origin"); got != repo.src {
							t.Fatalf("%s origin = %q, want %q", repo.path, got, repo.src)
						}
						if got := runTestGit(t, ctx, dir, "remote", "get-url", "--push", "origin"); got != "https://example.invalid/owner/publish.git" {
							t.Fatalf("%s push origin = %q", repo.path, got)
						}
						if got := runTestGit(t, ctx, dir, "rev-parse", "main@{upstream}"); got != runTestGit(t, ctx, dir, "rev-parse", "main") {
							t.Fatalf("%s upstream = %q", repo.path, got)
						}
						refs := []string{"refs/heads/feature", "refs/heads/main"}
						if tc.tagRegexp != "" {
							refs = append(refs, "refs/tags/v-base", "refs/tags/v-main")
						} else if got := runTestGit(t, ctx, dir, "tag", "--list"); got != "" {
							t.Fatalf("tags were pushed with no selection: %s", got)
						}
						for _, ref := range refs {
							if got, want := runTestGit(t, ctx, dir, "rev-parse", ref), runTestGit(t, ctx, repo.src, "rev-parse", ref); got != want {
								t.Fatalf("%s %s = %s, want %s", repo.path, ref, got, want)
							}
						}
						if got := runTestGit(t, ctx, dir, "tag", "--list", "ignored"); got != "" {
							t.Fatalf("excluded tag was pushed: %s", got)
						}
						runTestGit(t, ctx, dir, "fsck", "--connectivity-only", "--no-reflogs")
						if got := runTestGit(t, ctx, dir, "rev-parse", "--is-shallow-repository"); got != strconv.FormatBool(shallow) {
							t.Fatalf("%s shallow = %s, want %t", repo.path, got, shallow)
						}
					}
					// Repeated synchronization must work after Git sets module worktrees.
					if err := ct.pushSubmodules(ctx, io.Discard, &stderr, filepath.ToSlash(dst), host, tc.tagRegexp, true); err != nil {
						t.Fatalf("repeat pushSubmodules: %v: %s", err, stderr.String())
					}
					upstream := filepath.Join(t.TempDir(), "origin.git")
					runTestGit(t, ctx, "", "clone", "-q", "--bare", host, upstream)
					runTestGit(t, ctx, host, "remote", "add", "origin", upstream)
					runTestGit(t, ctx, host, "fetch", "-q", "origin")
					runTestGit(t, ctx, host, "branch", "--set-upstream-to", "origin/main", "main")
					runTestGit(t, ctx, host, "remote", "add", ct.Name, dst)
					ct.Home = t.TempDir()
					writeTestSSHConfig(t, ct.Home)
					ct.Runtime = testRunningRuntime(t, ct.Logger)
					ct.Repos = []Repo{{
						GitRoot: host, ContainerPath: filepath.ToSlash(dst), Branches: []string{"main"},
						DefaultRemote: "origin", DefaultBranch: "main",
					}}
					// Embedded clones can be staged as undeclared Gitlinks.
					// In-place modules can still store metadata in their worktrees.
					for _, name := range []string{"embedded", "in-place"} {
						runTestGit(t, ctx, dst, "clone", "-q", module, name)
					}
					runTestGit(t, ctx, dst, "add", "embedded")
					runTestGit(t, ctx, dst, "submodule", "add", "-q", module, "in-place")
					tree, err := ct.DiscoverSubmodules(ctx, 0)
					if err != nil || len(tree) != 1 || len(tree[0].Children) != 1 {
						t.Fatalf("discover initialized module tree: %+v, %v", tree, err)
					}
					leaf, ok := tree.Find("dep/nested")
					if !ok || leaf.BranchName != "" {
						t.Fatalf("discover nested detached submodule: %+v", leaf)
					}
					child := filepath.Join(dst, "dep", "nested")
					writeTestFile(t, filepath.Join(child, "new.txt"), "detached pending work\n")
					snapshot, err := ct.FetchRecursive(ctx, io.Discard, &stderr, 0, nil)
					if err != nil {
						t.Fatal(err)
					}
					fetched, ok := snapshot.Submodules.Find("dep/nested")
					if !ok || fetched.CommitHash != nestedTip {
						t.Fatalf("fetch committed detached HEAD: %+v, %v: %s", fetched, err, stderr.String())
					}
					if got := runTestGit(t, ctx, child, "status", "--porcelain"); !strings.Contains(got, "new.txt") {
						t.Fatalf("read-only fetch changed worktree: %q", got)
					}
					// The exact HEAD and Gitlink remain reachable, and no
					// temporary transfer ref survives.
					retained := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "for-each-ref", "--format=%(refname) %(objectname)", ct.submoduleFetchRef(""), "refs/md/fetch/")
					if want := ct.submoduleFetchRef("head") + " " + nestedTip + "\n" + ct.submoduleFetchRef("pins/"+nestedTip) + " " + nestedTip; retained != want {
						t.Fatalf("retained refs:\n%s\nwant:\n%s", retained, want)
					}
					// A container-only module must get an object store without a
					// host checkout or an invented branch.
					if err := os.Rename(leaf.GitDir, filepath.Join(t.TempDir(), "old-module")); err != nil {
						t.Fatal(err)
					}
					t.Setenv(fakeSSHFailureMatchEnv, " config --null --list")
					t.Setenv(fakeSSHFailureStatusEnv, "1")
					if _, err := ct.FetchRecursive(ctx, io.Discard, &stderr, 0, nil); err == nil {
						t.Fatal("interrupted module initialization succeeded")
					}
					if _, err := os.Stat(filepath.Join(leaf.GitDir, "HEAD")); !os.IsNotExist(err) {
						t.Fatalf("partial module initialization retained: %v", err)
					}
					t.Setenv(fakeSSHFailureMatchEnv, "")
					snapshot, err = ct.FetchRecursive(ctx, io.Discard, &stderr, 0, &FetchOpts{Commit: true})
					if err != nil {
						t.Fatalf("fetch pending detached work: %v: %s", err, stderr.String())
					}
					fetched, ok = snapshot.Submodules.Find("dep/nested")
					if !ok || fetched.CommitHash == nestedTip || fetched.BranchName != "" {
						t.Fatalf("pending work was not fetched detached: %+v", fetched)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "for-each-ref", "--format=%(refname)", "refs/heads/"); got != "" {
						t.Fatalf("fetch invented host branch %q", got)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "remote", "get-url", "origin"); got != nested {
						t.Fatalf("missing host publication remote: %q", got)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
						t.Fatalf("missing publication default branch: %q", got)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "rev-parse", "refs/remotes/origin/main"); got != runTestGit(t, ctx, child, "rev-parse", "refs/remotes/origin/main") {
						t.Fatalf("missing cached upstream: %q", got)
					}
					parent := snapshot.Submodules[0]
					if got := runTestGit(t, ctx, host, "--git-dir="+parent.GitDir, "rev-parse", parent.CommitHash+":nested"); got != fetched.CommitHash {
						t.Fatalf("parent did not capture fetched child: %s", got)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "show", fetched.CommitHash+":new.txt"); got != "detached pending work" {
						t.Fatalf("fetched content = %q", got)
					}
					// A rewritten module HEAD need not contain the parent's pinned
					// commit; collect both object graphs into a fresh host store.
					pin := fetched.CommitHash
					runTestGit(t, ctx, child, "checkout", "-q", "--orphan", "rewrite")
					runTestGit(t, ctx, child, "rm", "-q", "-rf", ".")
					runTestGit(t, ctx, child, "commit", "-q", "--allow-empty", "-m", "rewritten module")
					if err := os.Rename(fetched.GitDir, filepath.Join(t.TempDir(), "fetched-module")); err != nil {
						t.Fatal(err)
					}
					snapshot, err = ct.FetchRecursive(ctx, io.Discard, &stderr, 0, nil)
					if err != nil {
						t.Fatalf("fetch rewritten HEAD and pinned object: %v: %s", err, stderr.String())
					}
					fetched, ok = snapshot.Submodules.Find("dep/nested")
					edge := FetchedGitlink{Path: "dep/nested", ParentCommit: parent.CommitHash, CommitHash: pin}
					if !ok || !slices.Contains(snapshot.Gitlinks, edge) || fetched.CommitHash == pin {
						t.Fatalf("rewritten module snapshots: %+v, %+v", fetched, snapshot.Gitlinks)
					}
					if got := runTestGit(t, ctx, host, "--git-dir="+fetched.GitDir, "show", pin+":new.txt"); got != "detached pending work" {
						t.Fatalf("pinned object unavailable: %q", got)
					}
					if tc.name == "full" {
						// The staged module's path is still a directory in the
						// committed parent. It becomes a pin after the commit.
						parentPath := filepath.Join(dst, "dep")
						if err := os.Mkdir(filepath.Join(parentPath, "replacement"), 0o700); err != nil {
							t.Fatal(err)
						}
						writeTestFile(t, filepath.Join(parentPath, "replacement", "old.txt"), "old directory\n")
						runTestGit(t, ctx, parentPath, "add", ".")
						runTestGit(t, ctx, parentPath, "commit", "-qm", "directory before module")
						before := runTestGit(t, ctx, parentPath, "rev-parse", "HEAD")
						runTestGit(t, ctx, parentPath, "rm", "-qr", "replacement")
						source := newRepo("replacement-source")
						runTestGit(t, ctx, parentPath, "submodule", "add", "-q", "--name", "new-module", source, "replacement")
						want := runTestGit(t, ctx, source, "rev-parse", "HEAD")
						for _, opts := range []*FetchOpts{nil, {Commit: true}} {
							result, err := ct.FetchRecursive(ctx, io.Discard, &stderr, 0, opts)
							if err != nil {
								t.Fatalf("fetch directory replacement: %v: %s", err, stderr.String())
							}
							const replacement = "dep/replacement"
							item, ok := result.Submodules.Find(replacement)
							if !ok || item.CommitHash != want {
								t.Fatalf("directory replacement snapshot: %+v", item)
							}
							edge := FetchedGitlink{Path: replacement, ParentCommit: result.Submodules[0].CommitHash, CommitHash: want}
							if got := slices.Contains(result.Gitlinks, edge); got != (opts != nil) {
								t.Fatalf("directory replacement edge retained = %t with opts %+v", got, opts)
							}
							if opts == nil && runTestGit(t, ctx, parentPath, "rev-parse", "HEAD") != before {
								t.Fatal("read-only fetch committed directory replacement")
							}
						}
					}
				}) {
					return
				}
				//nolint:paralleltest // Rejection requires the repository populated by the valid subtest.
				t.Run("error", func(t *testing.T) {
					ctx := t.Context()
					// Submodule pushes must not overwrite commits made in the container.
					dep := filepath.Join(dst, "dep")
					runTestGit(t, ctx, dep, "checkout", "-q", "feature")
					runTestGit(t, ctx, dep, "commit", "-q", "--allow-empty", "-m", "container work")
					tip := runTestGit(t, ctx, dep, "rev-parse", "HEAD")
					stderr.Reset()
					if err := ct.pushSubmodules(ctx, io.Discard, &stderr, filepath.ToSlash(dst), host, tc.tagRegexp, true); err == nil || !strings.Contains(err.Error(), "push submodule refs dep") || !strings.Contains(stderr.String(), "rejected") {
						t.Fatalf("pushSubmodules must report rejection: %v: %s", err, stderr.String())
					}
					if got := runTestGit(t, ctx, dep, "rev-parse", "feature"); got != tip {
						t.Fatalf("container feature was overwritten: %s, want %s", got, tip)
					}
				})
			})
		}
	})
	t.Run("pushRefspecs_skips_host_hooks", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("Git hooks require a POSIX executable in this test")
		}
		ctx := t.Context()
		hostDir := t.TempDir()
		remoteDir := filepath.Join(t.TempDir(), "container.git")
		hooksDir := filepath.Join(hostDir, "hooks")

		runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
		runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
		writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "main\n")
		runTestGit(t, ctx, hostDir, "add", ".")
		runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
		if err := os.Mkdir(hooksDir, 0o750); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(hooksDir, "pre-push"), "#!/bin/sh\nexit 1\n")
		// #nosec G302 -- Git requires hooks to be executable.
		if err := os.Chmod(filepath.Join(hooksDir, "pre-push"), 0o700); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, ctx, hostDir, "config", "core.hooksPath", hooksDir)

		ct := &Container{Client: testClient(t), Logger: testLogger(t)}
		if err := ct.pushRefspecs(ctx, hostDir, nil, remoteDir, []string{"main:main"}, false, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/heads/main"); got != runTestGit(t, ctx, hostDir, "rev-parse", "main") {
			t.Errorf("pushed main = %q, want %q", got, runTestGit(t, ctx, hostDir, "rev-parse", "main"))
		}
	})
	t.Run("pushMappedBranchRefs_shallow", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		src := t.TempDir()
		hostDir := filepath.Join(t.TempDir(), "shallow")
		containerDir := filepath.Join(t.TempDir(), "container.git")
		runTestGit(t, ctx, src, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, src, "commit", "-q", "--allow-empty", "-m", "base")
		runTestGit(t, ctx, src, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, src, "tag", "v1")
		runTestGit(t, ctx, src, "checkout", "-q", "-b", "feature")
		runTestGit(t, ctx, src, "commit", "-q", "--allow-empty", "-m", "feature")
		runTestGit(t, ctx, src, "checkout", "-q", "main")
		// Disable local object copying so Git honors --depth on every platform.
		runTestGit(t, ctx, "", "clone", "-q", "--no-local", "--depth=1", "--no-single-branch", src, hostDir)
		runTestGit(t, ctx, "", "init", "-q", "--bare", containerDir)
		runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", "task", "--track", "origin/main")
		runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)
		ct := &Container{Client: testClient(t), Logger: testLogger(t), Name: "md-test"}
		r := &Repo{GitRoot: hostDir, Branches: []string{"task"}, Remotes: []string{"origin"}, TagRegexp: ".*", DefaultRemote: "origin", DefaultBranch: "main"}
		for _, step := range []string{"initial", "new_shallow_root"} {
			if step == "new_shallow_root" {
				runTestGit(t, ctx, src, "commit", "-q", "--allow-empty", "-m", "next")
				runTestGit(t, ctx, hostDir, "fetch", "-q", "--depth=1", "origin")
				runTestGit(t, ctx, hostDir, "commit", "-q", "--allow-empty", "-m", "task work")
			}
			var stderr bytes.Buffer
			if _, err := ct.pushMappedBranchRefs(ctx, io.Discard, &stderr, r); err != nil {
				t.Fatalf("%s push: %v: %s", step, err, stderr.String())
			}
			for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/feature", "refs/tags/v1"} {
				if got, want := runTestGit(t, ctx, containerDir, "rev-parse", ref), runTestGit(t, ctx, hostDir, "rev-parse", ref); got != want {
					t.Fatalf("%s %s = %s, want %s", step, ref, got, want)
				}
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "--is-shallow-repository"); got != "true" {
				t.Fatalf("container is shallow = %s", got)
			}
			runTestGit(t, ctx, containerDir, "fsck", "--connectivity-only", "--no-reflogs")
		}
		// A task tip ahead of its upstream must arrive through an incoming seed.
		if got, want := runTestGit(t, ctx, containerDir, "rev-parse", "refs/md/incoming/task"), runTestGit(t, ctx, hostDir, "rev-parse", "task"); got != want {
			t.Fatalf("incoming task = %s, want %s", got, want)
		}
		if snapshot, err := seedSource(ctx, hostDir); err != nil || snapshot.objectsDir != "" {
			t.Fatalf("shallow object copy must use Git transfer: %+v, %v", snapshot, err)
		}
	})
	t.Run("SyncDefaultBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("local_only_default_branch", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "migration")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "migration\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "migration")
			migrationCommit := runTestGit(t, ctx, dir, "rev-parse", "migration")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-1")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "task\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "task")
			runTestGit(t, ctx, dir, "config", "branch.caic-1.remote", ".")
			runTestGit(t, ctx, dir, "config", "branch.caic-1.merge", "refs/heads/migration")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"caic-1"},
					DefaultRemote: "origin",
					DefaultBranch: "migration",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/migration"); got != migrationCommit {
				t.Errorf("pushed origin/migration = %q, want %q", got, migrationCommit)
			}
			var stdout, stderr bytes.Buffer
			if _, err := ct.pushMappedBranchRefs(ctx, &stdout, &stderr, &ct.Repos[0]); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stderr.String(), "deleting a non-existent ref") {
				t.Errorf("push mapped branches emitted deletion warning:\n%s", stderr.String())
			}
		})
		t.Run("all_remote_branches", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
			upstreamPushDir := filepath.Join(t.TempDir(), "upstream-push.git")
			containerDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamPushDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", containerDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
			runTestGit(t, ctx, dir, "push", "-q", "origin", "main", "main:maintenance")
			runTestGit(t, ctx, dir, "remote", "set-head", "origin", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "release")
			writeTestFile(t, filepath.Join(dir, "release.txt"), "release\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "release")
			runTestGit(t, ctx, dir, "push", "-q", "upstream", "release")
			runTestGit(t, ctx, dir, "branch", "--set-upstream-to=upstream/release", "release")
			runTestGit(t, ctx, dir, "remote", "set-url", "--push", "upstream", upstreamPushDir)
			runTestGit(t, ctx, dir, "tag", "release-1")
			runTestGit(t, ctx, dir, "tag", "internal-1", "main")
			runTestGit(t, ctx, dir, "tag", "-a", "hidden-annotated", "-m", "hidden", "main")
			runTestGit(t, ctx, dir, "config", "push.followTags", "true")
			originCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/main")
			maintenanceCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/maintenance")
			upstreamCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/upstream/release")
			releaseTag := runTestGit(t, ctx, dir, "rev-parse", "refs/tags/release-1")

			repo := Repo{GitRoot: dir, Branches: []string{"release"}, TagRegexp: "^release-"}
			logger := testLogger(t)
			if err := repo.ResolveDefaults(ctx, logger); err != nil {
				t.Fatal(err)
			}
			if want := []string{"origin", "upstream"}; !slices.Equal(repo.Remotes, want) {
				t.Fatalf("remotes = %v, want %v", repo.Remotes, want)
			}

			containerWorktree := t.TempDir()
			runTestGit(t, ctx, containerWorktree, "init", "-q")
			runTestGit(t, ctx, dir, "push", "-q", containerWorktree,
				"refs/remotes/origin/main:refs/remotes/origin/main",
				"refs/remotes/upstream/release:refs/remotes/upstream/release")
			repo.ContainerPath = filepath.ToSlash(containerWorktree)
			configs, err := repo.containerRemoteConfigs(ctx, logger)
			if err != nil {
				t.Fatal(err)
			}
			commands := containerRemoteConfigCommands(&repo, configs)
			commands = append(commands, containerBranchSetupCommands(
				[]containerBranchBase{{branch: "release", ref: "upstream/release", upstreamRemote: "upstream", upstreamBranch: "release", pushRemote: "origin"}},
			)...)
			cmd := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test temp paths
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("configure container remotes: %v\n%s", err, out)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "origin"); !sameTestPath(got, originDir) {
				t.Errorf("container origin URL = %q, want %q", got, originDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "upstream"); !sameTestPath(got, upstreamDir) {
				t.Errorf("container upstream URL = %q, want %q", got, upstreamDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "--push", "upstream"); !sameTestPath(got, upstreamPushDir) {
				t.Errorf("container upstream push URL = %q, want %q", got, upstreamPushDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); got != "upstream/release" {
				t.Errorf("container upstream = %q, want upstream/release", got)
			}
			if got := runTestGit(t, ctx, containerWorktree, "for-each-ref", "--format=%(push:remotename)", "refs/heads/release"); got != "origin" {
				t.Errorf("container push remote = %q, want origin", got)
			}
			// A removed host remote must not erase the usable container URL or
			// prevent its cached refs from being synchronized.
			runTestGit(t, ctx, dir, "config", "--remove-section", "remote.upstream")
			configs, err = repo.containerRemoteConfigs(ctx, logger)
			if err != nil {
				t.Fatal(err)
			}
			cmd = exec.CommandContext(ctx, "bash", "-c", strings.Join(containerRemoteConfigCommands(&repo, configs), " && ")) //nolint:gosec // commands are generated from test temp paths
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("refresh container remotes: %v\n%s", err, out)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "upstream"); !sameTestPath(got, upstreamDir) {
				t.Errorf("preserved upstream URL = %q, want %q", got, upstreamDir)
			}
			if got := runTestGit(t, ctx, containerWorktree, "remote", "get-url", "--push", "upstream"); !sameTestPath(got, upstreamPushDir) {
				t.Errorf("preserved upstream push URL = %q, want %q", got, upstreamPushDir)
			}

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   containerDir,
				Repos:  []Repo{repo},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/origin/main"); got != originCommit {
				t.Errorf("pushed origin/main = %q, want %q", got, originCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/origin/maintenance"); got != maintenanceCommit {
				t.Errorf("pushed origin/maintenance = %q, want %q", got, maintenanceCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/remotes/upstream/release"); got != upstreamCommit {
				t.Errorf("pushed upstream/release = %q, want %q", got, upstreamCommit)
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/remotes/origin/HEAD"); err == nil {
				t.Error("container origin/HEAD exists, want it omitted")
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/tags/release-1"); got != releaseTag {
				t.Errorf("pushed release-1 tag = %q, want %q", got, releaseTag)
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/tags/internal-1"); err == nil {
				t.Error("container internal-1 tag exists, want it filtered out")
			}
			if _, err := (&git.Checkout{Root: containerDir, Logger: logger}).RevParse(ctx, "refs/tags/hidden-annotated"); err == nil {
				t.Error("container hidden-annotated tag exists via push.followTags, want it filtered out")
			}

			noTagsDir := filepath.Join(t.TempDir(), "no-tags.git")
			runTestGit(t, ctx, "", "init", "-q", "--bare", noTagsDir)
			noTagsRepo := repo
			noTagsRepo.TagRegexp = ""
			noTagsContainer := &Container{
				Client: testClient(t),
				Logger: logger,
				Name:   noTagsDir,
				Repos:  []Repo{noTagsRepo},
			}
			if err := noTagsContainer.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := (&git.Checkout{Root: noTagsDir, Logger: logger}).RevParse(ctx, "refs/tags/hidden-annotated"); err == nil {
				t.Error("default mapping included hidden-annotated tag, want no tags")
			}

			allTagsDir := filepath.Join(t.TempDir(), "all-tags.git")
			runTestGit(t, ctx, "", "init", "-q", "--bare", allTagsDir)
			allTagsRepo := repo
			allTagsRepo.TagRegexp = ".*"
			allTagsContainer := &Container{
				Client: testClient(t),
				Logger: logger,
				Name:   allTagsDir,
				Repos:  []Repo{allTagsRepo},
			}
			if err := allTagsContainer.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, allTagsDir, "rev-parse", "refs/tags/internal-1"); got != originCommit {
				t.Errorf("pushed all-tags internal-1 = %q, want %q", got, originCommit)
			}
		})
		t.Run("default_branch_also_updates_origin_ref", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "remote main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "remote main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
			remoteCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/main")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"main"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/main"); got != remoteCommit {
				t.Errorf("pushed origin/main = %q, want %q", got, remoteCommit)
			}
		})
		t.Run("tracked_branch_origin_ref_uses_upstream", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			originDir := filepath.Join(t.TempDir(), "origin.git")
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "remote feature\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "remote feature")
			runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "feature")
			remoteFeatureCommit := runTestGit(t, ctx, dir, "rev-parse", "refs/remotes/origin/feature")
			writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local feature\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "local feature")
			localFeatureCommit := runTestGit(t, ctx, dir, "rev-parse", "feature")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"feature"},
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			if err := ct.SyncDefaultBranch(ctx, 0); err != nil {
				t.Fatal(err)
			}
			gotFeatureCommit := runTestGit(t, ctx, remoteDir, "rev-parse", "refs/remotes/origin/feature")
			if gotFeatureCommit != remoteFeatureCommit {
				t.Errorf("pushed origin/feature = %q, want upstream %q", gotFeatureCommit, remoteFeatureCommit)
			}
			if gotFeatureCommit == localFeatureCommit {
				t.Errorf("pushed origin/feature = local unpushed commit %q", gotFeatureCommit)
			}
		})
		t.Run("missing_mapped_branch_is_rejected", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			remoteDir := filepath.Join(t.TempDir(), "container.git")

			runTestGit(t, ctx, "", "init", "-q", "--bare", remoteDir)
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "config", "user.name", "Test")
			runTestGit(t, ctx, dir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
			runTestGit(t, ctx, dir, "add", ".")
			runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, dir, "checkout", "-q", "-b", "multiple_branches")
			writeTestFile(t, filepath.Join(dir, "README.md"), "deleted branch\n")
			runTestGit(t, ctx, dir, "commit", "-q", "-am", "deleted branch")
			runTestGit(t, ctx, dir, "checkout", "-q", "main")
			runTestGit(t, ctx, dir, "branch", "-D", "multiple_branches")

			ct := &Container{
				Client: testClient(t),
				Logger: testLogger(t),
				Name:   remoteDir,
				Repos: []Repo{{
					GitRoot:       dir,
					Branches:      []string{"multiple_branches"},
					DefaultRemote: "origin",
					DefaultBranch: "multiple_branches",
				}},
			}
			err := ct.SyncDefaultBranch(ctx, 0)
			if err == nil || !strings.Contains(err.Error(), `mapped host branch "multiple_branches" no longer exists`) {
				t.Fatalf("SyncDefaultBranch error = %v, want deleted host branch error", err)
			}
		})
	})
	t.Run("resolveContainerBranchBase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		upstreamDir := filepath.Join(t.TempDir(), "upstream.git")

		runTestGit(t, ctx, "", "init", "-q", "--bare", originDir)
		runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "main\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
		runTestGit(t, ctx, dir, "push", "-q", "origin", "main")
		runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", "main")
		runTestGit(t, ctx, dir, "config", "remote.pushDefault", "origin")

		// Simulate a persisted Repo label from before Remotes was added.
		repo := Repo{GitRoot: dir, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}
		logger := testLogger(t)
		if err := repo.ResolveDefaults(ctx, logger); err != nil {
			t.Fatal(err)
		}
		if want := []string{"origin", "upstream"}; !slices.Equal(repo.Remotes, want) {
			t.Fatalf("legacy remotes = %v, want %v", repo.Remotes, want)
		}
		base, err := repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != "upstream/main" || base.destination != "refs/remotes/upstream/main" || base.pushRemote != "origin" {
			t.Fatalf("remote base = %+v, want upstream/main with origin push remote", base)
		}
		source := repo
		source.DefaultRemote = "upstream"
		source.Remotes = nil
		sourcePreflight, err := preflightForkRepos(ctx, logger, []Repo{source}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := sourcePreflight.sourceUpstreams[0], (branchUpstreamSpec{remote: "upstream", branch: "main", pushRemote: "origin"}); got != want {
			t.Errorf("fork source upstream = %+v, want %+v", got, want)
		}
		for _, refspec := range []string{
			"refs/remotes/upstream/main:refs/remotes/upstream/main",
			"refs/remotes/origin/main:refs/remotes/origin/main",
		} {
			if !slices.Contains(sourcePreflight.sourceSyncRefspec[0], refspec) {
				t.Errorf("fork source refspecs = %v, want %q", sourcePreflight.sourceSyncRefspec[0], refspec)
			}
		}
		preflight, err := preflightForkRepos(ctx, logger, nil, []Repo{repo}, []Repo{{Branches: []string{"main-0"}}})
		if err != nil {
			t.Fatal(err)
		}
		extraBase := preflight.extraBases[0][0]
		if extraBase.branch != "main-0" || extraBase.ref != containerIncomingRefPrefix+"main-0" || extraBase.destination != containerIncomingRefPrefix+"main-0" || extraBase.upstreamRemote != "upstream" || extraBase.upstreamBranch != "main" || extraBase.pushRemote != "origin" {
			t.Fatalf("fork extra base = %+v, want incoming main-0 with upstream/main and origin push remote", extraBase)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "local main\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "local main")
		base, err = repo.resolveContainerBranchBase(ctx, logger, "main")
		if err != nil {
			t.Fatal(err)
		}
		if base.ref != containerIncomingRefPrefix+"main" || base.upstreamRemote != "upstream" || base.upstreamBranch != "main" || base.destination != containerIncomingRefPrefix+"main" {
			t.Fatalf("local base = %+v, want incoming main tracking upstream/main", base)
		}
		hostTip := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/main")
		containerDir := t.TempDir()
		runTestGit(t, ctx, containerDir, "init", "-q")
		runTestGit(t, ctx, dir, "push", "-q", containerDir, base.source+":"+base.destination)
		setup := exec.CommandContext(ctx, "bash", "-c", strings.Join(containerBranchSetupCommands([]containerBranchBase{base}), " && ")) //nolint:gosec // command is generated from test temp paths and fixed branch names
		setup.Dir = containerDir
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("set up branch from incoming ref: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/main"); got != hostTip {
			t.Errorf("container branch tip = %q, want host branch tip", got)
		}
		if got := runTestGit(t, ctx, containerDir, "for-each-ref", "--format=%(refname)", containerIncomingRefPrefix); got != "" {
			t.Errorf("incoming refs after branch setup = %q, want none", got)
		}

		runTestGit(t, ctx, dir, "branch", "--unset-upstream", "main")
		if _, err := repo.resolveContainerBranchBase(ctx, logger, "main"); err == nil || !strings.Contains(err.Error(), `mapped host branch "main" has no upstream`) {
			t.Fatalf("resolveContainerBranchBase error = %v, want missing upstream error", err)
		}
	})
	t.Run("Pull", func(t *testing.T) {
		t.Run("rejects_existing_rebase", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, _ := setupPullTest(t)
			previousTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")

			rebase := exec.CommandContext(ctx, "git", "rebase", "--exec", "git rev-parse --verify refs/heads/md-test-stop-rebase", "origin/main")
			rebase.Dir = hostDir
			rebase.Env = append(os.Environ(), "LANG=C")
			if out, err := rebase.CombinedOutput(); err == nil {
				t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain"); got != "" {
				t.Fatalf("pre-existing rebase status = %q, want clean", got)
			}
			rebaseHead := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD")

			err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
			if err == nil || !strings.Contains(err.Error(), "rebase is already in progress") {
				t.Fatalf("Pull error = %v, want existing rebase error", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD"); got != rebaseHead {
				t.Fatalf("HEAD = %q, want unchanged rebase HEAD %q", got, rebaseHead)
			}
			if active, err := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); err != nil {
				t.Fatal(err)
			} else if !active {
				t.Fatal("pre-existing rebase was aborted")
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != previousTip {
				t.Fatalf("tracking ref = %q, want unchanged %q", got, previousTip)
			}
		})
		t.Run("rejects_dirty_host_worktree", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			previousTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main")
			writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "dirty host\n")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
			if err == nil || !strings.Contains(err.Error(), "pending changes on branch main locally") {
				t.Fatalf("Pull error = %v, want pending changes on branch main", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != previousTip {
				t.Fatalf("tracking ref = %q, want unchanged previous container tip", got)
			}
		})
		t.Run("allows_untracked_host_files", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			untrackedPath := filepath.Join(hostDir, "scratch.txt")
			writeTestFile(t, untrackedPath, "local scratch\n")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull with untracked host file: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
			contents, err := os.ReadFile(untrackedPath) //nolint:gosec // path belongs to the test's temporary directory.
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", untrackedPath, err)
			}
			if got := string(contents); got != "local scratch\n" {
				t.Fatalf("scratch.txt = %q, want preserved untracked contents", got)
			}
		})
		t.Run("retains_container_only_submodule_commits", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			// newSource returns a repository that never receives the container's
			// submodule commits, so a host checkout can only find them in what
			// Pull transferred.
			newSource := func(t *testing.T) string {
				ctx := t.Context()
				source := t.TempDir()
				runTestGit(t, ctx, source, "init", "-q", "--initial-branch=main")
				runTestGit(t, ctx, source, "commit", "-q", "--allow-empty", "-m", "source")
				return source
			}
			addModule := func(t *testing.T, parent, link string) string {
				ctx := t.Context()
				runTestGit(t, ctx, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", newSource(t), link)
				dir := filepath.Join(parent, link)
				runTestGit(t, ctx, dir, "config", "user.name", "Test")
				runTestGit(t, ctx, dir, "config", "user.email", "test@test")
				return dir
			}
			// commitAll commits content in dir and the new Gitlinks in each
			// superproject up to the container root, then returns dir's HEAD.
			commitAll := func(t *testing.T, content string, dirs ...string) string {
				ctx := t.Context()
				writeTestFile(t, filepath.Join(dirs[0], "work.txt"), content)
				for _, dir := range dirs {
					runTestGit(t, ctx, dir, "add", ".")
					runTestGit(t, ctx, dir, "commit", "-q", "-m", "container only")
				}
				return runTestGit(t, ctx, dirs[0], "rev-parse", "HEAD")
			}
			setup := func(t *testing.T) (ct *Container, hostDir string, pin func(content string) string) {
				ct, hostDir, containerDir := setupPullTest(t)
				dep := addModule(t, containerDir, "dep")
				return ct, hostDir, func(content string) string { return commitAll(t, content, dep, containerDir) }
			}
			pull := func(t *testing.T, ct *Container) string {
				var stderr bytes.Buffer
				if err := ct.Pull(t.Context(), io.Discard, &stderr, RepoTarget{}, nil); err != nil {
					t.Fatalf("Pull: %v\n%s", err, stderr.String())
				}
				return stderr.String()
			}
			// assertCheckout verifies that the host pins and checks out commit at
			// the submodule path, as git submodule update --init would.
			assertCheckout := func(t *testing.T, hostDir, link, commit string) {
				ctx := t.Context()
				if got := runTestGit(t, ctx, hostDir, "rev-parse", "main:"+link); got != commit {
					t.Fatalf("host gitlink %s = %s, want %s", link, got, commit)
				}
				if got := runTestGit(t, ctx, filepath.Join(hostDir, filepath.FromSlash(link)), "rev-parse", "HEAD"); got != commit {
					t.Fatalf("host submodule %s HEAD = %s, want %s", link, got, commit)
				}
				if got := runTestGit(t, ctx, hostDir, "status", "--porcelain", "--untracked-files=no"); got != "" {
					t.Fatalf("host status after checkout = %q", got)
				}
			}
			// legacyStore integrates the pinning superproject commit without md and
			// adds the bare store that earlier md versions created, without the pin.
			legacyStore := func(t *testing.T, hostDir string) {
				ctx := t.Context()
				runTestGit(t, ctx, hostDir, "pull", "-q", "--ff-only", "md-test", "main")
				store := filepath.Join(hostDir, ".git", "modules", "dep")
				runTestGit(t, ctx, "", "init", "-q", "--bare", store)
				source := runTestGit(t, ctx, hostDir, "config", "--file", ".gitmodules", "submodule.dep.url")
				runTestGit(t, ctx, "", "--git-dir="+store, "fetch", "-q", source, "+refs/heads/*:refs/remotes/origin/*")
			}
			t.Run("fresh_host", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ct, hostDir, pin := setup(t)
				// The host has never initialized the submodule.
				first := pin("first\n")
				pull(t, ct)
				assertCheckout(t, hostDir, "dep", first)
				// The host store exists and already holds the pin.
				pull(t, ct)
				assertCheckout(t, hostDir, "dep", first)
				// The host store exists and lacks the new pin.
				second := pin("second\n")
				pull(t, ct)
				assertCheckout(t, hostDir, "dep", second)
			})
			t.Run("nested", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ct, hostDir, containerDir := setupPullTest(t)
				dep := addModule(t, containerDir, "dep")
				leaf := addModule(t, dep, "leaf")
				first := commitAll(t, "first\n", leaf, dep, containerDir)
				pull(t, ct)
				depPin := runTestGit(t, t.Context(), dep, "rev-parse", "HEAD")
				assertCheckout(t, hostDir, "dep", depPin)
				if got := runTestGit(t, t.Context(), filepath.Join(hostDir, "dep", "leaf"), "rev-parse", "HEAD"); got != first {
					t.Fatalf("host nested submodule HEAD = %s, want %s", got, first)
				}
				second := commitAll(t, "second\n", leaf, dep, containerDir)
				pull(t, ct)
				if got := runTestGit(t, t.Context(), filepath.Join(hostDir, "dep", "leaf"), "rev-parse", "HEAD"); got != second {
					t.Fatalf("host nested submodule HEAD = %s, want %s", got, second)
				}
			})
			t.Run("unmapped_checkout", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ctx := t.Context()
				ct, hostDir, pin := setup(t)
				// Pull then updates only refs, so it leaves the worktree alone.
				runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "other")
				first := pin("first\n")
				pull(t, ct)
				if got := runTestGit(t, ctx, hostDir, "rev-parse", "main:dep"); got != first {
					t.Fatalf("host gitlink = %s, want %s", got, first)
				}
				if _, err := os.Stat(filepath.Join(hostDir, "dep", ".git")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("submodule checked out on an unmapped branch: %v", err)
				}
			})
			t.Run("leaves_local_work", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ctx := t.Context()
				ct, hostDir, pin := setup(t)
				first := pin("first\n")
				pull(t, ct)
				hostDep := filepath.Join(hostDir, "dep")
				// Pull refuses a dirty host tree unless the submodule is ignored.
				runTestGit(t, ctx, hostDir, "config", "submodule.dep.ignore", "all")
				runTestGit(t, ctx, hostDep, "config", "user.name", "Test")
				runTestGit(t, ctx, hostDep, "config", "user.email", "test@test")
				writeTestFile(t, filepath.Join(hostDep, "work.txt"), "host change\n")
				pin("second\n")
				if got := pull(t, ct); !strings.Contains(got, "has local changes") {
					t.Fatalf("Pull stderr = %q, want a local changes notice", got)
				}
				if got := runTestGit(t, ctx, hostDep, "rev-parse", "HEAD"); got != first {
					t.Fatalf("dirty host submodule moved to %s", got)
				}
				runTestGit(t, ctx, hostDep, "commit", "-q", "-am", "host commit")
				moved := runTestGit(t, ctx, hostDep, "rev-parse", "HEAD")
				pin("third\n")
				if got := pull(t, ct); !strings.Contains(got, "not at the previous pin") {
					t.Fatalf("Pull stderr = %q, want a moved HEAD notice", got)
				}
				if got := runTestGit(t, ctx, hostDep, "rev-parse", "HEAD"); got != moved {
					t.Fatalf("moved host submodule changed to %s, want %s", got, moved)
				}
			})
			t.Run("legacy_bare_store", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ct, hostDir, pin := setup(t)
				first := pin("first\n")
				legacyStore(t, hostDir)
				pull(t, ct)
				assertCheckout(t, hostDir, "dep", first)
			})
			t.Run("legacy_bare_store_after_failed_update", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
				ctx := t.Context()
				ct, hostDir, pin := setup(t)
				first := pin("first\n")
				legacyStore(t, hostDir)
				runTestGit(t, ctx, hostDir, "submodule", "init")
				// The update fails on the bare store after adding core.worktree to
				// it, which breaks every host command that recurses into dep.
				update := exec.CommandContext(ctx, "git", "-c", "protocol.file.allow=always", "submodule", "update")
				status := exec.CommandContext(ctx, "git", "status", "--porcelain")
				for _, cmd := range []*exec.Cmd{update, status} {
					cmd.Dir = hostDir
					if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "core.bare and core.worktree do not make sense") {
						t.Fatalf("%q = %v, %s; want the bare store failure", cmd.Args, err, out)
					}
				}
				pull(t, ct)
				assertCheckout(t, hostDir, "dep", first)
			})
		})
		t.Run("uses_fully_qualified_remote_tracking_ref", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")
			runTestGit(t, ctx, hostDir, "branch", "md-test/main", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull with shadowing local branch: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != containerTip {
				t.Fatalf("host main = %q, want container tip %q", got, containerTip)
			}
		})
		t.Run("uses_fully_qualified_local_branch_ref", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			runTestGit(t, ctx, hostDir, "fetch", "-q", "md-test", "main")
			runTestGit(t, ctx, hostDir, "tag", "main", "refs/remotes/md-test/main")
			runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "other", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull with shadowing tag: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "host" {
				t.Fatalf("host.txt = %q, want preserved host commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "other" {
				t.Fatalf("current branch = %q, want restored other", got)
			}
		})
		t.Run("restores_detached_head", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			originalHead := runTestGit(t, ctx, hostDir, "rev-parse", "origin/main")
			runTestGit(t, ctx, hostDir, "switch", "-q", "--detach", originalHead)

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull from detached HEAD: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached HEAD", got)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "HEAD"); got != originalHead {
				t.Fatalf("HEAD = %q, want restored %q", got, originalHead)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "host" {
				t.Fatalf("host.txt = %q, want preserved host commit", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:container.txt"); got != "container" {
				t.Fatalf("container.txt = %q, want integrated container commit", got)
			}
		})
		t.Run("leaves_conflicting_rebase_for_manual_resolution", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
			writeTestFile(t, filepath.Join(hostDir, "shared.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host")
			hostTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container")
			runTestGit(t, ctx, hostDir, "switch", "-q", "-c", "other", "origin/main")

			var stderr bytes.Buffer
			err := ct.Pull(ctx, io.Discard, &stderr, RepoTarget{}, nil)
			if err == nil || !strings.Contains(err.Error(), "rebasing branch main") {
				t.Fatalf("Pull error = %v, want named rebase failure; stderr:\n%s", err, stderr.String())
			}
			if active, checkErr := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); checkErr != nil {
				t.Fatal(checkErr)
			} else if !active {
				t.Fatal("failed Pull aborted the conflicting rebase")
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached rebase checkout", got)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != hostTip {
				t.Fatalf("main tip = %q, want unresolved rebase branch tip %q", got, hostTip)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain", "--untracked-files=no"); got != "UU shared.txt" {
				t.Fatalf("tracked status after failed Pull = %q, want unresolved shared.txt conflict", got)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
				t.Fatalf("sync point after failed Pull = %q, want unchanged %q", got, previousSync)
			}
		})
		t.Run("later_branch_failure_keeps_all_integration_points", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupMultiBranchPullTest(t)
			runTestGit(t, ctx, containerDir, "branch", "feature", "origin/feature")
			previousSync := make(map[string]string, 2)
			for _, branch := range []string{"main", "feature"} {
				previousSync[branch] = runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/"+branch)
				runTestGit(t, ctx, containerDir, "update-ref", containerSyncRefPrefix+branch, previousSync[branch])
			}

			writeTestFile(t, filepath.Join(containerDir, "container-main.txt"), "container main\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			runTestGit(t, ctx, containerDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature.txt"), "container feature\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container feature")
			runTestGit(t, ctx, containerDir, "switch", "-q", "main")

			runTestGit(t, ctx, hostDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "host feature\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host feature")
			runTestGit(t, ctx, hostDir, "switch", "-q", "main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err == nil || !strings.Contains(err.Error(), "rebasing branch feature") {
				t.Fatalf("Pull error = %v, want feature rebase failure", err)
			}
			for _, branch := range []string{"main", "feature"} {
				if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+branch); got != previousSync[branch] {
					t.Errorf("%s integration point = %q, want unchanged %q", branch, got, previousSync[branch])
				}
			}
		})
		t.Run("integrates_unchanged_tracking_tip_after_host_reset", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull after host reset: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("host main = %q, want container tip %q", got, containerTip)
			}
		})
		t.Run("fast_forwards_after_previous_pull", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("second Pull: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container v2" {
				t.Fatalf("host container.txt = %q, want container v2", got)
			}
		})
		t.Run("replaces_amended_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull after amend: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:shared.txt"); got != "container v2" {
				t.Fatalf("host shared.txt = %q, want container v2", got)
			}
		})
		t.Run("rebases_host_commits_after_amended_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host change\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host change")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "container v2\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "container v2")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull after amend: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:shared.txt"); got != "container v2" {
				t.Fatalf("host shared.txt = %q, want container v2", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:host.txt"); got != "host change" {
				t.Fatalf("host host.txt = %q, want host change", got)
			}
		})
		t.Run("rebases_divergent_host_history_after_container_rewrite", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			runTestGit(t, ctx, hostDir, "reset", "--hard", "origin/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "divergent host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "divergent host")
			writeTestFile(t, filepath.Join(containerDir, "shared.txt"), "rewritten container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "--amend", "-am", "rewritten container")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull with independently divergent host history: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:shared.txt"); got != "rewritten container" {
				t.Fatalf("shared.txt = %q, want rewritten container content", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "refs/heads/main:host.txt"); got != "divergent host" {
				t.Fatalf("host.txt = %q, want preserved divergent host commit", got)
			}
		})
		t.Run("does_not_update_container_branch", func(t *testing.T) { //nolint:paralleltest // fakeSSH changes process environment.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			containerPath := filepath.ToSlash(containerDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container")
			containerCommit := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					ContainerPath: containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerCommit {
				t.Fatalf("container main = %q, want unchanged %q", got, containerCommit)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container" {
				t.Fatalf("host container.txt = %q, want container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:host.txt"); got != "host" {
				t.Fatalf("host host.txt = %q, want host", got)
			}
		})
		t.Run("commits_uncommitted_changes", func(t *testing.T) {
			ctx := t.Context()
			fakeSSH(t)
			sshLogPath := filepath.Join(t.TempDir(), "ssh.log")
			t.Setenv(fakeSSHLogEnv, sshLogPath)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "md")
			runTestGit(t, ctx, containerDir, "config", "user.email", "<>")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main"},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			fetched, err := ct.Fetch(ctx, &stdout, &stderr, 0, &FetchOpts{Commit: true})
			if err != nil {
				t.Fatalf("Fetch: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if len(fetched) != 1 || fetched[0].BranchName != "main" || fetched[0].CommitHash == "" {
				t.Fatalf("Fetch result = %+v, want main branch commit", fetched)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "md-test/main:container.txt"); got != "container" {
				t.Fatalf("fetched container.txt = %q, want container", got)
			}
			if got := runTestGit(t, ctx, containerDir, "show", "-s", "--format=%an <%ae>%n%cn <%ce>"); got != "Test <test@test>\nTest <test@test>" {
				t.Fatalf("container commit identity = %q, want host identity for author and committer", got)
			}
			sshLog, err := os.ReadFile(sshLogPath) //nolint:gosec // test log is under t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			sshCommands := strings.FieldsFunc(string(sshLog), func(r rune) bool { return r == '\n' })
			if len(sshCommands) > 1 {
				t.Fatalf("ssh invocations = %d, want at most 1\n%s", len(sshCommands), sshLog)
			}
			if !strings.Contains(string(sshLog), "git commit -a -q") {
				t.Fatalf("missing commit in ssh command:\n%s", sshLog)
			}
		})
		t.Run("hooks_reject_container_commit", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			if runtime.GOOS == "windows" {
				t.Skip("Git hooks require a POSIX executable in this test")
			}
			ctx := t.Context()
			ct, _, containerDir := setupPullTest(t)
			installRejectingHooks(t, containerDir)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil)
			if err == nil || !strings.Contains(err.Error(), "committing in container") {
				t.Fatalf("Pull with rejecting hooks error = %v, want a committing error", err)
			}
		})
		t.Run("no_verify_skips_container_hooks", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			if runtime.GOOS == "windows" {
				t.Skip("Git hooks require a POSIX executable in this test")
			}
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			installRejectingHooks(t, containerDir)
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			if err := ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, &PullOpts{NoVerify: true}); err != nil {
				t.Fatalf("Pull with NoVerify: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:container.txt"); got != "container" {
				t.Fatalf("host container.txt = %q, want container", got)
			}
		})
		t.Run("multiple_mapped_branches", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "main\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "main")

			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "main-container.txt"), "main container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature-container.txt"), "feature container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container feature")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(io.MultiWriter(&log, testLogWriter{t: t}), testLoggerOptions()))
			ct := &Container{
				Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
				Logger: logger,
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			if err := ct.Pull(ctx, &stdout, &stderr, RepoTarget{}, nil); err != nil {
				t.Fatalf("Pull: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			fetches := regexp.MustCompile(`cmd="\[git fetch -q [^\]]*\bmd-test (\+refs/heads/[^\]]*)\]"`).FindAllStringSubmatch(log.String(), -1)
			if len(fetches) != 1 {
				t.Fatalf("container fetch invocations = %d, want 1\n%s", len(fetches), log.String())
			}
			if got := regexp.MustCompile(`\+refs/heads/(\S+):`).FindAllStringSubmatch(fetches[0][1], -1); len(got) != 2 || got[0][1] != "main" || got[1][1] != "feature" {
				t.Fatalf("batched mapped-branch fetch = %q, want main and feature", fetches[0][1])
			}
			if got := runTestGit(t, ctx, hostDir, "show", "main:main-container.txt"); got != "main container" {
				t.Fatalf("main branch container file = %q, want main container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "show", "feature:feature-container.txt"); got != "feature container" {
				t.Fatalf("feature branch container file = %q, want feature container", got)
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "main" {
				t.Fatalf("current branch = %q, want main", got)
			}
		})
		t.Run("preserves_completed_branches_and_later_conflicting_rebase", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupMultiBranchPullTest(t)

			runTestGit(t, ctx, hostDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "host feature\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host feature")
			hostFeatureTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/feature")
			runTestGit(t, ctx, hostDir, "switch", "-q", "main")

			writeTestFile(t, filepath.Join(containerDir, "main-container.txt"), "main container\n")
			runTestGit(t, ctx, containerDir, "add", ".")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container main")
			containerMainTip := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/main")
			runTestGit(t, ctx, containerDir, "switch", "-q", "feature")
			writeTestFile(t, filepath.Join(containerDir, "feature.txt"), "container feature\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "container feature")
			containerFeatureTip := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/feature")
			runTestGit(t, ctx, containerDir, "switch", "-q", "main")

			var stderr bytes.Buffer
			err := ct.Pull(ctx, io.Discard, &stderr, RepoTarget{}, nil)
			if err == nil || !strings.Contains(err.Error(), "integrating branch feature") {
				t.Fatalf("Pull error = %v, want feature integration failure; stderr:\n%s", err, stderr.String())
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main"); got != containerMainTip {
				t.Fatalf("main tip = %q, want completed integration %q", got, containerMainTip)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/feature"); got != hostFeatureTip {
				t.Fatalf("feature tip = %q, want pre-rebase host tip %q", got, hostFeatureTip)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/feature"); got != containerFeatureTip {
				t.Fatalf("feature tracking tip = %q, want fetched container tip %q", got, containerFeatureTip)
			}
			if active, checkErr := gitRebaseInProgress(ctx, &git.Checkout{Root: hostDir, Logger: ct.Logger}); checkErr != nil {
				t.Fatal(checkErr)
			} else if !active {
				t.Fatal("failed Pull aborted the feature rebase")
			}
			if got := runTestGit(t, ctx, hostDir, "branch", "--show-current"); got != "" {
				t.Fatalf("current branch = %q, want detached feature rebase checkout", got)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--porcelain", "--untracked-files=no"); got != "UU feature.txt" {
				t.Fatalf("tracked status after partial Pull = %q, want unresolved feature.txt conflict", got)
			}
		})
		t.Run("deleted_source_branch_is_rejected", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			writeTestSSHConfig(t, home)

			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			branch := "multiple_branches"
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "README.md"), "base\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "base")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", branch)
			writeTestFile(t, filepath.Join(hostDir, "feature.txt"), "feature\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "feature")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", branch)
			runTestGit(t, ctx, "", "clone", "-q", originDir, containerDir)
			runTestGit(t, ctx, containerDir, "checkout", "-q", branch)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(containerDir, "container.txt"), "container\n")

			runTestGit(t, ctx, hostDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "push", "-q", "origin", "--delete", branch)
			runTestGit(t, ctx, hostDir, "branch", "-D", branch)
			runTestGit(t, ctx, hostDir, "update-ref", "-d", "refs/remotes/origin/"+branch)
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			ct := &Container{
				Client: &Client{Home: home, Logger: testLogger(t), Runtime: testRunningRuntime(t, testLogger(t))},
				Logger: testLogger(t),
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{branch},
					ContainerPath: filepath.ToSlash(containerDir),
					DefaultRemote: "origin",
					DefaultBranch: branch,
				}},
			}
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{name: "Diff", run: func() error { return ct.Diff(ctx, io.Discard, io.Discard, RepoTarget{}, nil) }},
				{name: "Pull", run: func() error { return ct.Pull(ctx, io.Discard, io.Discard, RepoTarget{}, nil) }},
			} {
				err := op.run()
				if err == nil || !strings.Contains(err.Error(), `mapped host branch "multiple_branches" no longer exists`) {
					t.Errorf("%s error = %v, want deleted host branch error", op.name, err)
				}
			}
		})
	})
	t.Run("Push", func(t *testing.T) {
		t.Run("skips_backups_for_preserved_history", func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
			for _, advance := range []bool{false, true} {
				name := "unchanged"
				if advance {
					name = "fast_forward"
				}
				t.Run(name, func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
					ctx := t.Context()
					ct, hostDir, containerDir := setupMultiBranchPullTest(t)
					runTestGit(t, ctx, containerDir, "branch", "feature", "origin/feature")
					if advance {
						for _, branch := range ct.Repos[0].Branches {
							runTestGit(t, ctx, hostDir, "switch", "-q", branch)
							writeTestFile(t, filepath.Join(hostDir, branch+".txt"), "host advance\n")
							runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "host advance")
						}
					}
					backup, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{})
					if err != nil {
						t.Fatal(err)
					}
					if backup != "" {
						t.Fatalf("backup = %q, want none", backup)
					}
					if got := runTestGit(t, ctx, containerDir, "for-each-ref", "--format=%(refname)", "refs/heads/backup-*"); got != "" {
						t.Fatalf("unexpected backup refs: %s", got)
					}
					for _, branch := range ct.Repos[0].Branches {
						want := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/"+branch)
						for _, ref := range []string{"refs/heads/" + branch, containerSyncRefPrefix + branch} {
							if got := runTestGit(t, ctx, containerDir, "rev-parse", ref); got != want {
								t.Fatalf("%s = %s, want %s", ref, got, want)
							}
						}
					}
				})
			}
		})
		t.Run("backs_up_lost_history", func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
			for _, scenario := range []string{"ahead", "diverged", "dirty", "detached", "unmapped_branch"} {
				t.Run(scenario, func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
					ctx := t.Context()
					ct, hostDir, containerDir := setupPullTest(t)
					switch scenario {
					case "detached":
						runTestGit(t, ctx, containerDir, "switch", "-q", "--detach")
					case "unmapped_branch":
						runTestGit(t, ctx, containerDir, "switch", "-q", "-c", "other")
					}
					writeTestFile(t, filepath.Join(containerDir, "work.txt"), "container work\n")
					if scenario != "dirty" {
						runTestGit(t, ctx, containerDir, "add", ".")
						runTestGit(t, ctx, containerDir, "commit", "-q", "-m", "container work")
					}
					if scenario == "diverged" {
						writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host work\n")
						runTestGit(t, ctx, hostDir, "add", ".")
						runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host work")
					}
					backup, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{})
					if err != nil {
						t.Fatal(err)
					}
					if !strings.HasPrefix(backup, "backup-") {
						t.Fatalf("backup = %q, want backup branch", backup)
					}
					if got := runTestGit(t, ctx, containerDir, "show", backup+":work.txt"); got != "container work" {
						t.Fatalf("backed-up work = %q", got)
					}
					want := runTestGit(t, ctx, hostDir, "rev-parse", "main")
					if got := runTestGit(t, ctx, containerDir, "rev-parse", "HEAD"); got != want {
						t.Fatalf("HEAD = %s, want %s", got, want)
					}
				})
			}
		})

		t.Run("backs_up_checked_out_local_upstream", func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
			for _, dirty := range []bool{false, true} {
				name := "committed"
				if dirty {
					name = "dirty"
				}
				t.Run(name, func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
					ctx := t.Context()
					ct, hostDir, containerDir := setupPullTest(t)
					runTestGit(t, ctx, hostDir, "branch", "base", "main")
					runTestGit(t, ctx, hostDir, "branch", "--set-upstream-to=base", "main")
					runTestGit(t, ctx, containerDir, "switch", "-q", "-c", "base")
					runTestGit(t, ctx, containerDir, "config", "receive.denyCurrentBranch", "ignore")
					if dirty {
						writeTestFile(t, filepath.Join(containerDir, "work.txt"), "upstream work\n")
					} else {
						runTestGit(t, ctx, containerDir, "commit", "-q", "--allow-empty", "-m", "upstream work")
					}
					tip := runTestGit(t, ctx, containerDir, "rev-parse", "HEAD")
					var log bytes.Buffer
					ct.Logger = slog.New(slog.NewJSONHandler(&log, testLoggerOptions()))
					backup, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{})
					if err != nil {
						t.Fatal(err)
					}
					if !strings.HasPrefix(backup, "backup-") {
						t.Fatalf("backup = %q, want backup branch", backup)
					}
					if dirty {
						if got := runTestGit(t, ctx, containerDir, "show", backup+":work.txt"); got != "upstream work" {
							t.Fatalf("backed-up work = %q", got)
						}
					} else if got := runTestGit(t, ctx, containerDir, "rev-parse", backup); got != tip {
						t.Fatalf("backup = %s, want original HEAD %s", got, tip)
					}
					want := runTestGit(t, ctx, hostDir, "rev-parse", "main")
					if got := runTestGit(t, ctx, containerDir, "rev-parse", "HEAD"); got != want {
						t.Fatalf("HEAD = %s, want %s", got, want)
					}
					if got := runTestGit(t, ctx, containerDir, "rev-parse", "refs/heads/base"); got != want {
						t.Fatalf("upstream = %s, want host base %s", got, want)
					}
					if got := runTestGit(t, ctx, containerDir, "status", "--porcelain"); got != "" {
						t.Fatalf("checkout is dirty after push: %s", got)
					}
					if got := runTestGit(t, ctx, containerDir, "for-each-ref", "--format=%(refname)", containerIncomingUpstreamRefPrefix); got != "" {
						t.Fatalf("staged upstream refs remain: %s", got)
					}
					pushes, ssh := 0, 0
					dec := json.NewDecoder(&log)
					for dec.More() {
						var record struct {
							Cmd []string `json:"cmd"`
						}
						if err := dec.Decode(&record); err != nil {
							t.Fatal(err)
						}
						if len(record.Cmd) > 1 && record.Cmd[0] == "git" && record.Cmd[1] == "push" {
							pushes++
						}
						if len(record.Cmd) > 0 && record.Cmd[0] == "ssh" {
							ssh++
						}
					}
					// One transfer plus the existing integration-point push, and
					// the existing backup and checkout SSH commands.
					if pushes != 2 || ssh != 2 {
						t.Fatalf("pushes = %d, SSH commands = %d, want 2 each", pushes, ssh)
					}
				})
			}
		})

		t.Run("ancestry_error_aborts_reset", func(t *testing.T) { //nolint:paralleltest // setup uses t.Setenv.
			ctx := t.Context()
			ct, _, containerDir := setupMultiBranchPullTest(t)
			// The fixture has no local feature branch, so its ancestry cannot be checked.
			tip := runTestGit(t, ctx, containerDir, "rev-parse", "HEAD")
			_, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{})
			if err == nil || !strings.Contains(err.Error(), "backing up container state") {
				t.Fatalf("Push error = %v, want ancestry failure", err)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "HEAD"); got != tip {
				t.Fatalf("HEAD = %s, want unchanged %s", got, tip)
			}
			if got := runTestGit(t, ctx, containerDir, "for-each-ref", "--format=%(refname)", "refs/heads/backup-*"); got != "" {
				t.Fatalf("unexpected backup refs: %s", got)
			}
		})

		t.Run("rejects_staged_host_changes", func(t *testing.T) { //nolint:paralleltest // setupPullTest uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			writeTestFile(t, filepath.Join(hostDir, "staged.txt"), "staged\n")
			runTestGit(t, ctx, hostDir, "add", "staged.txt")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{}); err == nil || !strings.Contains(err.Error(), "pending changes on branch main locally") {
				t.Fatalf("Push error = %v, want pending host changes error", err)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("container main = %q, want unchanged %q", got, containerTip)
			}
			if got := runTestGit(t, ctx, hostDir, "status", "--short"); got != "A  staged.txt" {
				t.Fatalf("host status = %q, want staged change preserved", got)
			}
		})
		t.Run("failed_reset_keeps_previous_sync_point", func(t *testing.T) {
			ctx := t.Context()
			ct, hostDir, containerDir := setupPullTest(t)
			containerTip := runTestGit(t, ctx, containerDir, "rev-parse", "main")
			previousSync := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			t.Setenv(fakeSSHFailureMatchEnv, "git switch -q -C main")
			t.Setenv(fakeSSHFailureTextEnv, "ssh: simulated reset failure")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{}); err == nil {
				t.Fatal("Push error = nil, want reset failure")
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "main"); got != containerTip {
				t.Fatalf("container main = %q, want unchanged %q", got, containerTip)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", containerSyncRefPrefix+"main"); got != previousSync {
				t.Fatalf("sync point = %q, want previous %q", got, previousSync)
			}
		})
		t.Run("uses_fully_qualified_tracking_source", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			ct, hostDir, _ := setupPullTest(t)
			runTestGit(t, ctx, hostDir, "tag", "main", "origin/main")
			writeTestFile(t, filepath.Join(hostDir, "host.txt"), "host\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "host")
			hostTip := runTestGit(t, ctx, hostDir, "rev-parse", "refs/heads/main")

			if _, err := ct.Push(ctx, io.Discard, io.Discard, RepoTarget{}); err != nil {
				t.Fatalf("Push with shadowing tag: %v", err)
			}
			if got := runTestGit(t, ctx, hostDir, "rev-parse", "refs/remotes/md-test/main"); got != hostTip {
				t.Fatalf("tracking ref = %q, want host branch tip %q", got, hostTip)
			}
		})
		t.Run("backs_up_extra_branch_before_reset", func(t *testing.T) { //nolint:paralleltest // fakeSSH uses t.Setenv.
			ctx := t.Context()
			fakeSSH(t)
			home := t.TempDir()
			sshConfigDir := filepath.Join(home, ".ssh", "config.d")
			if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sshConfigDir, "md-test.conf"), []byte("Host md-test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			originDir := filepath.Join(t.TempDir(), "origin.git")
			hostDir := t.TempDir()
			containerDir := t.TempDir()
			containerPath := filepath.ToSlash(containerDir)
			runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
			runTestGit(t, ctx, hostDir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, hostDir, "config", "user.name", "Test")
			runTestGit(t, ctx, hostDir, "config", "user.email", "test@test")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "main\n")
			runTestGit(t, ctx, hostDir, "add", ".")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-m", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "origin", originDir)
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "main")
			runTestGit(t, ctx, hostDir, "checkout", "-q", "-b", "feature")
			writeTestFile(t, filepath.Join(hostDir, "tracked.txt"), "feature host\n")
			runTestGit(t, ctx, hostDir, "commit", "-q", "-am", "feature host")
			runTestGit(t, ctx, hostDir, "push", "-q", "-u", "origin", "feature")

			runTestGit(t, ctx, "", "clone", "-q", "--no-checkout", hostDir, containerDir)
			runTestGit(t, ctx, containerDir, "config", "user.name", "Test")
			runTestGit(t, ctx, containerDir, "config", "user.email", "test@test")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "-B", "main", "origin/main")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "-B", "feature", "origin/feature")
			writeTestFile(t, filepath.Join(containerDir, "tracked.txt"), "feature container\n")
			runTestGit(t, ctx, containerDir, "commit", "-q", "-am", "feature container")
			extraBranchCommit := runTestGit(t, ctx, containerDir, "rev-parse", "feature")
			runTestGit(t, ctx, containerDir, "checkout", "-q", "main")
			runTestGit(t, ctx, hostDir, "remote", "add", "md-test", containerDir)

			logger := testLogger(t)
			ct := &Container{
				Client: &Client{Home: home, Logger: logger, Runtime: testRunningRuntime(t, logger)},
				Logger: logger,
				Name:   "md-test",
				Repos: []Repo{{
					GitRoot:       hostDir,
					Branches:      []string{"main", "feature"},
					ContainerPath: containerPath,
					DefaultRemote: "origin",
					DefaultBranch: "main",
				}},
			}
			var stdout, stderr bytes.Buffer
			backupBranch, err := ct.Push(ctx, &stdout, &stderr, RepoTarget{})
			if err != nil {
				t.Fatalf("Push: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			extraBackup := backupBranch + "-1-feature"
			if got := runTestGit(t, ctx, containerDir, "rev-parse", extraBackup); got != extraBranchCommit {
				t.Fatalf("extra branch backup = %q, want %q", got, extraBranchCommit)
			}
			if got := runTestGit(t, ctx, containerDir, "rev-parse", "feature"); got == extraBranchCommit {
				t.Fatalf("feature branch was not reset from host")
			}
		})
	})
	t.Run("Fork", func(t *testing.T) {
		t.Run("host_branch_tracks_source_upstream", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name             string
				sourceRemote     string
				sourceMergeRef   string
				sourcePushRemote string
				wantRemote       string
				wantMergeRef     string
				wantPushRemote   string
			}{
				{name: "remote_branch", sourceRemote: "fork", sourceMergeRef: "refs/heads/main", sourcePushRemote: "origin", wantRemote: "fork", wantMergeRef: "refs/heads/main", wantPushRemote: "origin"},
				{name: "local_branch", sourceRemote: ".", sourceMergeRef: "refs/heads/main", wantRemote: ".", wantMergeRef: "refs/heads/main", wantPushRemote: "."},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					ctx := t.Context()
					dir := t.TempDir()
					runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
					runTestGit(t, ctx, dir, "config", "user.name", "Test")
					runTestGit(t, ctx, dir, "config", "user.email", "test@test")
					writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
					runTestGit(t, ctx, dir, "add", ".")
					runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
					runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
					runTestGit(t, ctx, dir, "remote", "add", "fork", "/dev/null")
					runTestGit(t, ctx, dir, "branch", "source")
					runTestGit(t, ctx, dir, "update-ref", "refs/remotes/fork/source", "source")
					runTestGit(t, ctx, dir, "config", "branch.autoSetupMerge", "always")
					runTestGit(t, ctx, dir, "config", "branch.autoSetupRebase", "always")
					if tt.sourceRemote != "" {
						runTestGit(t, ctx, dir, "config", "branch.source.remote", tt.sourceRemote)
						runTestGit(t, ctx, dir, "config", "branch.source.merge", tt.sourceMergeRef)
					}
					if tt.sourcePushRemote != "" {
						runTestGit(t, ctx, dir, "config", "branch.source.pushRemote", tt.sourcePushRemote)
					}

					repo := &Repo{GitRoot: dir, DefaultRemote: "origin", DefaultBranch: "main"}
					_, err := repo.createForkHostBranch(ctx, testLogger(t), "source-0", "fork/source", branchUpstreamSpec{remote: tt.wantRemote, branch: strings.TrimPrefix(tt.wantMergeRef, "refs/heads/"), pushRemote: tt.wantPushRemote})
					if err != nil {
						t.Fatal(err)
					}
					if got := runTestGit(t, ctx, dir, "rev-parse", "source-0"); got != runTestGit(t, ctx, dir, "rev-parse", "fork/source") {
						t.Errorf("fork tip = %q, want fork/source %q", got, runTestGit(t, ctx, dir, "rev-parse", "fork/source"))
					}
					if got := runTestGit(t, ctx, dir, "config", "--get", "branch.source-0.remote"); got != tt.wantRemote {
						t.Errorf("fork remote = %q, want %q", got, tt.wantRemote)
					}
					if got := runTestGit(t, ctx, dir, "config", "--get", "branch.source-0.merge"); got != tt.wantMergeRef {
						t.Errorf("fork merge ref = %q, want %q", got, tt.wantMergeRef)
					}
					gotPushRemote, _ := (&git.Checkout{Root: dir, Logger: testLogger(t)}).RunGit(ctx, "config", "--get", "branch.source-0.pushRemote")
					if gotPushRemote != tt.wantPushRemote {
						t.Errorf("fork push remote = %q, want %q", gotPushRemote, tt.wantPushRemote)
					}
					repo.Branches = []string{"source-0"}
					if _, err := repo.containerSyncRefspecs(ctx, testLogger(t), nil); err != nil {
						t.Fatalf("syncing fork host destination branch: %v", err)
					}
				})
			}
		})
	})
}

func TestRepoContainerHooksPath(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		hooksPath string
		want      string
	}{
		{name: "relative", hooksPath: "scripts/hooks", want: "scripts/hooks"},
		{name: "absolute", hooksPath: filepath.Join(t.TempDir(), "hooks")},
		{name: "home_relative", hooksPath: "~/.config/git/hooks"},
		{name: "escapes_repository", hooksPath: "../hooks"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runTestGit(t, t.Context(), dir, "init", "-q")
			runTestGit(t, t.Context(), dir, "config", "core.hooksPath", tt.hooksPath)

			got, err := (&Repo{GitRoot: dir}).containerHooksPath(t.Context(), testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("containerHooksPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFork(t *testing.T) {
	t.Parallel()

	t.Run("host_branch_rollback", func(t *testing.T) {
		t.Parallel()
		newRepo := func(t *testing.T) (Repo, string, string) {
			dir := t.TempDir()
			runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, t.Context(), dir, "config", "user.name", "Test")
			runTestGit(t, t.Context(), dir, "config", "user.email", "test@test")
			runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "base")
			base := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			runTestGit(t, t.Context(), dir, "commit", "-q", "--allow-empty", "-m", "fork")
			forkTip := runTestGit(t, t.Context(), dir, "rev-parse", "HEAD")
			return Repo{GitRoot: dir}, base, forkTip
		}

		t.Run("deletes_new_branch", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			if err := change.rollback(t.Context(), testLogger(t)); err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: repo.GitRoot, Logger: testLogger(t)}
			exists, err := g.RefExists(t.Context(), "refs/heads/forked")
			if err != nil {
				t.Fatal(err)
			}
			if exists {
				t.Fatal("new fork branch still exists after rollback")
			}
			if remote, err := optionalGitConfig(t.Context(), g, "branch.forked.remote"); err != nil || remote != "" {
				t.Fatalf("fork branch remote after rollback = %q, %v; want absent", remote, err)
			}
		})

		t.Run("restores_existing_branch_and_config", func(t *testing.T) {
			t.Parallel()
			repo, base, forkTip := newRepo(t)
			runTestGit(t, t.Context(), repo.GitRoot, "branch", "forked", base)
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.remote", "old-origin")
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.merge", "refs/heads/old-main")
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.pushRemote", "old-push")

			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: repo.GitRoot, Logger: testLogger(t)}
			if pushRemote, err := optionalGitConfig(t.Context(), g, "branch.forked.pushRemote"); err != nil || pushRemote != "" {
				t.Fatalf("fork branch push remote = %q, %v; want stale value cleared", pushRemote, err)
			}
			if err := change.rollback(t.Context(), testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != base {
				t.Fatalf("restored branch tip = %q, want %q", got, base)
			}
			for _, entry := range []struct {
				key  string
				want string
			}{
				{key: "branch.forked.merge", want: "refs/heads/old-main"},
				{key: "branch.forked.pushRemote", want: "old-push"},
				{key: "branch.forked.remote", want: "old-origin"},
			} {
				if got := runTestGit(t, t.Context(), repo.GitRoot, "config", "--get", entry.key); got != entry.want {
					t.Errorf("%s = %q, want %q", entry.key, got, entry.want)
				}
			}
		})

		t.Run("preserves_concurrently_moved_branch", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			runTestGit(t, t.Context(), repo.GitRoot, "commit", "-q", "--allow-empty", "-m", "concurrent")
			concurrentTip := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "HEAD")
			runTestGit(t, t.Context(), repo.GitRoot, "branch", "-f", "forked", concurrentTip)

			err = change.rollback(t.Context(), testLogger(t))
			if err == nil || !strings.Contains(err.Error(), "changed after fork wrote it") {
				t.Fatalf("rollback error = %v, want concurrent-change error", err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != concurrentTip {
				t.Fatalf("concurrently moved branch tip = %q, want preserved %q", got, concurrentTip)
			}
		})

		t.Run("preserves_concurrently_changed_config", func(t *testing.T) {
			t.Parallel()
			repo, _, forkTip := newRepo(t)
			change, err := repo.createForkHostBranch(t.Context(), testLogger(t), "forked", forkTip, branchUpstreamSpec{remote: "origin", branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			runTestGit(t, t.Context(), repo.GitRoot, "config", "branch.forked.remote", "concurrent-origin")

			err = change.rollback(t.Context(), testLogger(t))
			if err == nil || !strings.Contains(err.Error(), "configuration changed after fork wrote it") {
				t.Fatalf("rollback error = %v, want concurrent-config error", err)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "rev-parse", "forked"); got != forkTip {
				t.Fatalf("fork branch tip = %q, want preserved %q", got, forkTip)
			}
			if got := runTestGit(t, t.Context(), repo.GitRoot, "config", "--get", "branch.forked.remote"); got != "concurrent-origin" {
				t.Fatalf("fork branch remote = %q, want concurrent-origin", got)
			}
		})
	})

	t.Run("failed_attempt_rolls_back_artifacts", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		tip := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		const forkName = "md-repo-forked"
		runTestGit(t, ctx, dir, "remote", "add", forkName, "/dev/null")
		change, err := (&Repo{GitRoot: dir}).createForkHostBranch(ctx, testLogger(t), "forked", tip, branchUpstreamSpec{remote: "origin", branch: "main"})
		if err != nil {
			t.Fatal(err)
		}

		home := t.TempDir()
		configDir := filepath.Join(home, ".ssh", "config.d")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{forkName + ".conf", forkName + ".known_hosts"} {
			if err := os.WriteFile(filepath.Join(configDir, name), []byte("test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + logPath,
			fakeRuntimeStateEnv + "=running",
		}
		logger := testLogger(t)
		client := &Client{Home: home, Logger: logger, Runtime: testRuntime(t, executable, logger, env), env: env}
		source := &Container{Client: client, Logger: logger, Name: "md-source"}
		fork := &Container{
			Client: client,
			Logger: logger,
			Name:   forkName,
			Repos:  []Repo{{GitRoot: dir}},
			State:  "running",
		}
		attempt := forkAttempt{
			source:          source,
			fork:            fork,
			snapshotImage:   "md-fork-md-source",
			hostBranches:    []forkHostBranchChange{change},
			snapshotCreated: true,
		}
		if err := attempt.rollback(ctx); err != nil {
			t.Fatal(err)
		}

		g := &git.Checkout{Root: dir, Logger: logger}
		exists, err := g.RefExists(ctx, "refs/heads/forked")
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("fork host branch still exists")
		}
		if remotes := runTestGit(t, ctx, dir, "remote"); strings.Contains(remotes, forkName) {
			t.Errorf("fork remote still exists in %q", remotes)
		}
		for _, name := range []string{forkName + ".conf", forkName + ".known_hosts"} {
			if _, err := os.Stat(filepath.Join(configDir, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("SSH artifact %s still exists: %v", name, err)
			}
		}
		logData, err := os.ReadFile(logPath) //nolint:gosec // private test log.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"inspect " + forkName, "rm -f -v " + forkName, "rmi -f --no-prune md-fork-md-source"} {
			if !strings.Contains(string(logData), want) {
				t.Errorf("runtime log missing %q:\n%s", want, logData)
			}
		}
	})

	t.Run("local_primary_upstream_fails_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		originDir := filepath.Join(t.TempDir(), "origin.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch=main", originDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "origin", "main")
		runTestGit(t, ctx, dir, "branch", "feature")
		runTestGit(t, ctx, dir, "config", "branch.feature.remote", ".")
		runTestGit(t, ctx, dir, "config", "branch.feature.merge", "refs/heads/main")
		runTestGit(t, ctx, dir, "remote", "add", "md-test", "/dev/null")

		home := t.TempDir()
		writeTestSSHConfig(t, home)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       dir,
				Branches:      []string{"main", "feature"},
				ContainerPath: "/home/user/src/repo",
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		_, err = ct.Fork(ctx, io.Discard, io.Discard, &ForkOpts{
			Quiet: true,
			Repos: []ForkRepo{{GitRoot: dir, DestPrimary: "main-0"}},
		})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "feature" tracks primary branch "main" locally`) {
			t.Fatalf("Fork error = %v, want local primary-upstream error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(logData), "commit") || strings.Contains(string(logData), "create") || strings.Contains(string(logData), "run") {
			t.Fatalf("Fork mutated runtime before preflight failure:\n%s", logData)
		}
	})

	t.Run("missing_branch_fails_before_runtime_mutation", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "md-test", "/dev/null")
		home := t.TempDir()
		writeTestSSHConfig(t, home)
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(t.TempDir(), "runtime.log")
		env := []string{fakeRuntimeEnv + "=1", fakeRuntimeLogEnv + "=" + logPath}
		logger := testLogger(t)
		ct := &Container{
			Client: &Client{Home: home, Logger: logger, Runtime: testRuntime(t, exe, logger, env), env: env},
			Logger: logger,
			Name:   "md-test",
			Repos: []Repo{{
				GitRoot:       dir,
				Branches:      []string{"missing"},
				ContainerPath: "/home/user/src/repo",
				DefaultRemote: "origin",
				DefaultBranch: "main",
			}},
		}
		_, err = ct.Fork(ctx, io.Discard, io.Discard, &ForkOpts{
			Quiet: true,
			Repos: []ForkRepo{{GitRoot: dir, DestPrimary: "missing-0"}},
		})
		if err == nil || !strings.Contains(err.Error(), `mapped host branch "missing" no longer exists`) {
			t.Fatalf("Fork error = %v, want missing branch preflight error", err)
		}
		logData, readErr := os.ReadFile(logPath) //nolint:gosec // private test log.
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(logData), "commit") || strings.Contains(string(logData), "create") || strings.Contains(string(logData), "run") {
			t.Fatalf("fork mutated runtime before preflight failure:\n%s", logData)
		}
	})

	t.Run("memory_inheritance", func(t *testing.T) {
		t.Parallel()
		for _, source := range []uint64{0, 4 << 30} {
			opts := &ForkOpts{}
			if got := opts.startOptions(source).MaxMemory; got != source {
				t.Fatalf("inherited memory = %d, want %d", got, source)
			}
			for _, limit := range []uint64{0, 8 << 30} {
				opts.MaxMemory = &limit
				if got := opts.startOptions(source).MaxMemory; got != limit {
					t.Fatalf("overridden memory = %d, want %d", got, limit)
				}
			}
		}
	})

	t.Run("valid_options_do_not_inherit_privileges", func(t *testing.T) {
		t.Parallel()
		got := (&ForkOpts{}).startOptions(0)
		if got.MaxMemory != 0 {
			t.Fatalf("default fork memory cap = %d", got.MaxMemory)
		}
		if got.Display || got.tailscale() || got.USB || got.Sudo {
			t.Fatalf("fork options inherited disabled features: display=%v tailscale=%v usb=%v sudo=%v", got.Display, got.tailscale(), got.USB, got.Sudo)
		}
		for _, want := range []string{"MD_DISPLAY=", "MD_TAILSCALE=", "TAILSCALE_AUTHKEY=", "MD_SUDO_PASSWORD="} {
			if !slices.Contains(got.ExtraRunArgs, want) {
				t.Fatalf("fork run args missing %q in %v", want, got.ExtraRunArgs)
			}
		}

		limit := uint64(12 << 30)
		got = (&ForkOpts{Display: true, TailscaleAPIKey: "tskey-api-k-s", USB: true, Sudo: true, MaxMemory: &limit}).startOptions(4 << 30) //nolint:gosec // G101: fake token
		if got.MaxMemory != 12<<30 {
			t.Fatalf("fork memory cap = %d, want 12 GiB", got.MaxMemory)
		}
		if !got.Display || !got.tailscale() || !got.USB || !got.Sudo {
			t.Fatalf("fork options did not enable requested features: display=%v tailscale=%v usb=%v sudo=%v", got.Display, got.tailscale(), got.USB, got.Sudo)
		}
		if len(got.ExtraRunArgs) != 0 {
			t.Fatalf("enabled fork has env-clearing run args: %v", got.ExtraRunArgs)
		}
	})

	t.Run("valid_primary_branch_setup_renames_branch", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-23")

		setup := exec.CommandContext(ctx, "bash", "-c", forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")) //nolint:gosec // command is generated from test branch names
		setup.Dir = dir
		setup.Env = append(os.Environ(), "LANG=C")
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("fork primary branch setup: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "caic-23-0" {
			t.Fatalf("current branch = %q, want caic-23-0", got)
		}
	})

	t.Run("valid_sync_refspecs_pair_every_branch", func(t *testing.T) {
		t.Parallel()
		got := branchSyncRefspecs([]string{"main", "topic"}, []string{"main-0", "topic-0"})
		want := []string{
			"+refs/heads/main:" + containerSyncRefPrefix + "main-0",
			"+refs/heads/topic:" + containerSyncRefPrefix + "topic-0",
		}
		if !slices.Equal(got, want) {
			t.Errorf("branchSyncRefspecs() = %q, want %q", got, want)
		}
	})
	t.Run("valid_fork_refreshes_sync_points_to_committed_tips", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-23")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")
		staleSyncCommit := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"caic-23", staleSyncCommit)
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "committed in source")
		primaryTip := runTestGit(t, ctx, dir, "rev-parse", "HEAD")
		runTestGit(t, ctx, dir, "branch", "topic")
		runTestGit(t, ctx, dir, "update-ref", containerSyncRefPrefix+"topic", staleSyncCommit)

		commands := make([]string, 1, 4)
		commands[0] = forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")
		commands = append(commands, forkSyncPointCommands("caic-23", []string{"caic-23-0", "topic"})...)
		rename := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test branch names
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err != nil {
			t.Fatalf("fork sync point refresh: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"caic-23-0"); got != primaryTip {
			t.Errorf("primary sync point = %q, want committed tip %q", got, primaryTip)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"topic"); got != primaryTip {
			t.Errorf("topic sync point = %q, want committed tip %q", got, primaryTip)
		}
		if got := runTestGit(t, ctx, dir, "for-each-ref", "--format=%(refname)", containerSyncRefPrefix+"caic-23"); got != "" {
			t.Errorf("old primary sync ref = %q, want none", got)
		}
	})
	t.Run("valid_fork_creates_sync_point_without_a_recorded_point", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=caic-23")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "base")

		commands := make([]string, 1, 3)
		commands[0] = forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")
		commands = append(commands, forkSyncPointCommands("caic-23", []string{"caic-23-0"})...)
		rename := exec.CommandContext(ctx, "bash", "-c", strings.Join(commands, " && ")) //nolint:gosec // commands are generated from test branch names
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err != nil {
			t.Fatalf("fork sync point refresh without a recorded point: %v\n%s", err, out)
		}
		if got, want := runTestGit(t, ctx, dir, "rev-parse", containerSyncRefPrefix+"caic-23-0"), runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23-0"); got != want {
			t.Errorf("new sync point = %q, want committed tip %q", got, want)
		}
	})
	t.Run("valid_primary_branch_setup_preserves_rebase", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "base")
		runTestGit(t, ctx, dir, "checkout", "-q", "-b", "caic-23")
		runTestGit(t, ctx, dir, "config", "branch.caic-23.remote", "origin")
		runTestGit(t, ctx, dir, "config", "branch.caic-23.merge", "refs/heads/main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "source\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "source")
		sourceTip := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23")
		runTestGit(t, ctx, dir, "checkout", "-q", "main")
		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "target\n")
		runTestGit(t, ctx, dir, "commit", "-q", "-am", "target")
		runTestGit(t, ctx, dir, "checkout", "-q", "caic-23")

		rebase := exec.CommandContext(ctx, "git", "rebase", "main")
		rebase.Dir = dir
		rebase.Env = append(os.Environ(), "LANG=C")
		if out, err := rebase.CombinedOutput(); err == nil {
			t.Fatalf("git rebase unexpectedly succeeded:\n%s", out)
		}

		rename := exec.CommandContext(ctx, "git", "branch", "-m", "caic-23", "caic-23-0")
		rename.Dir = dir
		rename.Env = append(os.Environ(), "LANG=C")
		if out, err := rename.CombinedOutput(); err == nil {
			t.Fatalf("git branch -m unexpectedly succeeded:\n%s", out)
		}

		setup := exec.CommandContext(ctx, "bash", "-c", forkPrimaryBranchSetupCommand("caic-23", "caic-23-0")) //nolint:gosec // command is generated from test branch names
		setup.Dir = dir
		setup.Env = append(os.Environ(), "LANG=C")
		if out, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("fork primary branch setup: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "rev-parse", "refs/heads/caic-23-0"); got != sourceTip {
			t.Fatalf("new branch tip = %q, want %q", got, sourceTip)
		}
		verifyOld := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "refs/heads/caic-23") //nolint:gosec // dir is a test temp repo
		verifyOld.Env = append(os.Environ(), "LANG=C")
		if out, err := verifyOld.CombinedOutput(); err == nil {
			t.Fatalf("old branch still exists:\n%s", out)
		}
		headName, err := os.ReadFile(filepath.Join(dir, ".git", "rebase-merge", "head-name")) //nolint:gosec // test temp repo metadata
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(headName)); got != "refs/heads/caic-23-0" {
			t.Fatalf("rebase head-name = %q, want refs/heads/caic-23-0", got)
		}

		writeTestFile(t, filepath.Join(dir, "tracked.txt"), "resolved\n")
		runTestGit(t, ctx, dir, "add", ".")
		cont := exec.CommandContext(ctx, "git", "rebase", "--continue")
		cont.Dir = dir
		cont.Env = append(os.Environ(), "LANG=C", "GIT_EDITOR=true")
		if out, err := cont.CombinedOutput(); err != nil {
			t.Fatalf("git rebase --continue: %v\n%s", err, out)
		}
		if got := runTestGit(t, ctx, dir, "branch", "--show-current"); got != "caic-23-0" {
			t.Fatalf("current branch after rebase = %q, want caic-23-0", got)
		}
		if got := runTestGit(t, ctx, dir, "config", "--get", "branch.caic-23-0.remote"); got != "origin" {
			t.Errorf("fork remote = %q, want origin", got)
		}
		if got := runTestGit(t, ctx, dir, "config", "--get", "branch.caic-23-0.merge"); got != "refs/heads/main" {
			t.Errorf("fork merge ref = %q, want refs/heads/main", got)
		}
	})

	t.Run("valid_snapshot_clears_runtime_state", func(t *testing.T) {
		t.Parallel()
		changes := forkSnapshotConfigChanges("md.sudo md.sudo-password md.image_type custom.label")
		for _, want := range []string{
			"LABEL md.sudo=",
			"LABEL md.sudo-password=",
			"LABEL custom.label=",
			"ENV MD_HOST_GID=",
			"ENV MD_HOST_UID=",
			"ENV MD_SUDO_PASSWORD=",
			"ENV MD_TAILSCALE=",
			"ENV TAILSCALE_AUTHKEY=",
			"LABEL md.image_type=fork-snapshot",
		} {
			if !slices.Contains(changes, want) {
				t.Fatalf("fork snapshot changes missing %q in %v", want, changes)
			}
		}
		// The fork-snapshot stamp must come after the inherited md.image_type is
		// cleared, otherwise the specialized value would win.
		cleared := slices.Index(changes, "LABEL md.image_type=")
		stamp := slices.Index(changes, "LABEL md.image_type=fork-snapshot")
		if cleared == -1 || stamp <= cleared {
			t.Fatalf("fork-snapshot stamp (%d) must follow the clearing change (%d): %v", stamp, cleared, changes)
		}
	})

	t.Run("valid_untags_image", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name string
			rt   string
			want string
		}{
			{name: "docker", rt: "docker", want: "rmi -f --no-prune md-fork-md-source"},
			{name: "podman", rt: "podman", want: "image untag md-fork-md-source"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				runtimeName := tt.rt
				if runtime.GOOS == "windows" {
					runtimeName += ".exe"
				}
				runtimePath := filepath.Join(dir, runtimeName)
				if err := linkOrCopyExecutable(exe, runtimePath); err != nil {
					t.Fatal(err)
				}
				logPath := filepath.Join(t.TempDir(), "runtime.log")
				logger := testLogger(t)
				env := []string{
					fakeRuntimeEnv + "=1",
					fakeRuntimeLogEnv + "=" + logPath,
				}
				ct := &Container{Client: &Client{
					Logger:  logger,
					Runtime: testRuntime(t, runtimePath, logger, env),
					env:     env,
				}}
				if err := ct.untagImage(t.Context(), "md-fork-md-source"); err != nil {
					t.Fatal(err)
				}
				logData, err := os.ReadFile(logPath) //nolint:gosec // logPath is a private test temp file.
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSpace(string(logData)); got != tt.want {
					t.Fatalf("runtime command = %q, want %q", got, tt.want)
				}
			})
		}
	})
}

func TestUnmarshalContainer(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
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
		t.Parallel()
		reposData, _ := json.Marshal([]Repo{{GitRoot: "/home/user/repo", Branches: []string{"main"}}})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `,md.tailscale_tags=tag:md tag:ci-2,other=ignored"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"tag:md", "tag:ci-2"}; !slices.Equal(ct.TailscaleTags, want) {
			t.Errorf("TailscaleTags = %q, want %q", ct.TailscaleTags, want)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if ct.Repos[0].GitRoot != "/home/user/repo" {
			t.Errorf("Repos[0].GitRoot = %q, want %q", ct.Repos[0].GitRoot, "/home/user/repo")
		}
		if len(ct.Repos[0].Branches) != 1 || ct.Repos[0].Branches[0] != "main" {
			t.Errorf("Repos[0].Branches = %v, want [main]", ct.Repos[0].Branches)
		}
	})
	t.Run("legacy_mounted_path_label", func(t *testing.T) {
		t.Parallel()
		reposData := []byte(`[{"git_root":"/home/user/repo","branches":["main"],"mounted_path":"/home/user/src/org/repo"}]`)
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := ct.Repos[0].ContainerPath; got != "/home/user/src/org/repo" {
			t.Errorf("ContainerPath = %q, want /home/user/src/org/repo", got)
		}
	})
	t.Run("legacy_branch_label", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runTestGit(t, t.Context(), dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, t.Context(), dir, "remote", "add", "origin", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "upstream", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "md-repo-main", "/dev/null")
		runTestGit(t, t.Context(), dir, "remote", "add", "md-other", "user@md-other:/home/user/src/"+filepath.Base(dir))
		reposData, err := json.Marshal([]map[string]string{{"git_root": dir, "branch": "main"}})
		if err != nil {
			t.Fatal(err)
		}
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), &Client{Logger: testLogger(t)}, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 1 {
			t.Fatalf("len(Repos) = %d, want 1", len(ct.Repos))
		}
		if !slices.Equal(ct.Repos[0].Branches, []string{"main"}) {
			t.Fatalf("Branches = %v, want [main]", ct.Repos[0].Branches)
		}
		if want := []string{"origin", "upstream"}; !slices.Equal(ct.Repos[0].Remotes, want) {
			t.Fatalf("Remotes = %v, want %v", ct.Repos[0].Remotes, want)
		}
	})
	t.Run("no_labels", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":""}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if ct.Repos != nil {
			t.Errorf("expected nil Repos, got %v", ct.Repos)
		}
	})
	t.Run("empty_repos", func(t *testing.T) {
		t.Parallel()
		// No-repo containers encode md.repos as an empty JSON array.
		reposData, _ := json.Marshal([]Repo{})
		reposB64 := base64.StdEncoding.EncodeToString(reposData)
		raw := `{"Names":"md-agent","State":"running","CreatedAt":"2025-06-15 10:30:00 +0000 UTC","Labels":"md.repos=` + reposB64 + `"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(ct.Repos) != 0 {
			t.Errorf("expected empty Repos, got %v", ct.Repos)
		}
	})
	t.Run("podman_rfc3339", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00.123456789Z"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 123456789, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_no_frac", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00Z"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("podman_rfc3339_offset", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"md-repo-main","State":"running","CreatedAt":"2025-06-15T10:30:00+02:00"}`
		ct, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		wantTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.FixedZone("", 2*60*60))
		if !ct.CreatedAt.Equal(wantTime) {
			t.Errorf("CreatedAt = %v, want %v", ct.CreatedAt, wantTime)
		}
	})
	t.Run("bad_created_at", func(t *testing.T) {
		t.Parallel()
		raw := `{"Names":"x","State":"running","CreatedAt":"not-a-date"}`
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte(raw))
		if err == nil {
			t.Fatal("expected error for bad CreatedAt")
		}
	})
	t.Run("empty_input", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("bad_json", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalContainer(t.Context(), testClient(t), []byte("{not json}"))
		if err == nil {
			t.Fatal("expected error for bad JSON")
		}
	})
}

func TestAllAgentPaths(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got := allAgentPaths(nil)
		if !slices.ContainsFunc(got, func(p AgentPaths) bool {
			return slices.Contains(p.XDGConfigPaths, "md") && p.ReadOnly
		}) {
			t.Errorf("allAgentPaths(nil) = %+v, want read-only md path", got)
		}
	})

	t.Run("prepend", func(t *testing.T) {
		t.Parallel()
		input := []AgentPaths{
			{HomePaths: []string{".foo"}, XDGConfigPaths: []string{"bar"}},
			{HomePaths: []string{".baz"}, LocalSharePaths: []string{"qux"}, ReadOnly: true},
		}
		got := allAgentPaths(input)
		if len(got) != len(alwaysPaths)+len(input) {
			t.Fatalf("len(allAgentPaths(input)) = %d, want %d", len(got), len(alwaysPaths)+len(input))
		}
		if !slices.Equal(got[len(alwaysPaths)].HomePaths, []string{".foo"}) || !slices.Equal(got[len(alwaysPaths)+1].LocalSharePaths, []string{"qux"}) {
			t.Errorf("allAgentPaths(input) = %+v, want input groups preserved after alwaysPaths", got)
		}
		if !got[len(alwaysPaths)+1].ReadOnly {
			t.Errorf("allAgentPaths(input) = %+v, want input ReadOnly preserved", got)
		}
	})

	t.Run("does_not_mutate_global", func(t *testing.T) {
		t.Parallel()
		before := len(alwaysPaths)
		_ = allAgentPaths([]AgentPaths{{XDGConfigPaths: []string{"extra1", "extra2"}}})
		after := len(alwaysPaths)
		if before != after {
			t.Errorf("alwaysPaths mutated: was %d, now %d", before, after)
		}
	})
}

func TestMount(t *testing.T) {
	t.Parallel()
	t.Run("dockerArg", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		hostDir := filepath.Join(home, "data")
		if err := os.MkdirAll(hostDir, 0o750); err != nil {
			t.Fatal(err)
		}
		mount := Mount{
			HostPath:      "~/data",
			ContainerPath: "~/data",
			ReadOnly:      true,
		}
		got, err := mount.dockerArg(home)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.ToSlash(hostDir) + ":/home/user/data:ro"
		if got != want {
			t.Errorf("dockerArg = %q, want %q", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		filePath := filepath.Join(home, "file")
		if err := os.WriteFile(filePath, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name  string
			mount Mount
		}{
			{name: "missing_host", mount: Mount{HostPath: filepath.Join(home, "missing"), ContainerPath: "/data"}},
			{name: "file_host", mount: Mount{HostPath: filePath, ContainerPath: "/data"}},
			{name: "empty_container", mount: Mount{HostPath: home}},
			{name: "relative_container", mount: Mount{HostPath: home, ContainerPath: "data"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if _, err := tt.mount.dockerArg(home); err == nil {
					t.Fatal("expected error")
				}
			})
		}
	})
}

func TestContainerInspect(t *testing.T) {
	t.Parallel()

	t.Run("valid configured CPU metadata", func(t *testing.T) {
		t.Parallel()
		runtimePath, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
			fakeRuntimeStateEnv + `=raw:[{"Platform":"linux/amd64","HostConfig":{"NanoCpus":1500000000,"CpusetCpus":"3,1-2"}}]`,
		}
		c := &Client{Logger: logger, Runtime: testRuntime(t, runtimePath, logger, env), env: env}
		info, err := (&Container{Client: c, Name: "md-test"}).Inspect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if info.CPULimit != 1.5 || !slices.Equal(info.CPUSet, []int{1, 2, 3}) {
			t.Fatalf("Inspect CPU metadata = %+v", info)
		}
	})

	t.Run("valid fills missing architecture", func(t *testing.T) {
		t.Parallel()
		runtimePath, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		logger := testLogger(t)
		env := []string{
			fakeRuntimeEnv + "=1",
			fakeRuntimeLogEnv + "=" + filepath.Join(t.TempDir(), "runtime.log"),
		}
		c := &Client{
			Logger:  logger,
			Runtime: testRuntime(t, runtimePath, logger, env),
			env:     env,
		}
		info, err := (&Container{Client: c, Name: "md-test"}).Inspect(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != "linux" || info.Architecture != "amd64" {
			t.Fatalf("Inspect OS/Architecture = %q/%q, want linux/amd64", info.OS, info.Architecture)
		}
	})
}

func TestFillFromInspect(t *testing.T) {
	t.Parallel()
	// Both Docker and Podman inspect return a JSON array.
	inspect := `[{
  "Name": "/md-caic-caic-3",
  "State": { "Status": "running" },
  "Created": "2025-06-15T10:30:00Z",
  "Config": {
    "Labels": {
      "md.display": "1",
      "md.tailscale": "1",
      "md.tailscale_tags": "tag:ci",
      "md.usb": "1",
      "md.sudo": "1",
      "custom": "value"
    }
  },
  "NetworkSettings": {
    "Ports": {
      "22/tcp": [{"HostPort": "32768"}],
      "5901/tcp": [{"HostPort": "32769"}]
    }
  }
}]`

	ct := &Container{Client: testClient(t)}
	if err := ct.fillFromInspect(t.Context(), []byte(inspect)); err != nil {
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
	if !slices.Equal(ct.TailscaleTags, []string{"tag:ci"}) {
		t.Errorf("TailscaleTags = %q, want [tag:ci]", ct.TailscaleTags)
	}
	if ct.Labels["custom"] != "value" {
		t.Errorf("custom label = %q, want value", ct.Labels["custom"])
	}
	if ct.SSHPort != 32768 || ct.VNCPort != 32769 {
		t.Errorf("ports = ssh %d vnc %d, want 32768/32769", ct.SSHPort, ct.VNCPort)
	}
	legacyDir := t.TempDir()
	runTestGit(t, t.Context(), legacyDir, "init", "-q", "--initial-branch=main")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "origin", "/dev/null")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "upstream", "/dev/null")
	runTestGit(t, t.Context(), legacyDir, "remote", "add", "md-legacy", "/dev/null")
	legacyReposData, err := json.Marshal([]map[string]string{{"git_root": legacyDir, "branch": "main"}})
	if err != nil {
		t.Fatal(err)
	}
	legacyReposB64 := base64.StdEncoding.EncodeToString(legacyReposData)
	legacyInspect := `[{"Name":"/md-legacy","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{"md.repos":"` + legacyReposB64 + `"}}}]`
	ctLegacy := &Container{Client: &Client{Logger: testLogger(t)}, Logger: testLogger(t)}
	if err := ctLegacy.fillFromInspect(t.Context(), []byte(legacyInspect)); err != nil {
		t.Fatalf("legacy inspect: %v", err)
	}
	if len(ctLegacy.Repos) != 1 {
		t.Fatalf("len(legacy Repos) = %d, want 1", len(ctLegacy.Repos))
	}
	if !slices.Equal(ctLegacy.Repos[0].Branches, []string{"main"}) {
		t.Fatalf("legacy Branches = %v, want [main]", ctLegacy.Repos[0].Branches)
	}
	if want := []string{"origin", "upstream"}; !slices.Equal(ctLegacy.Repos[0].Remotes, want) {
		t.Fatalf("legacy Remotes = %v, want %v", ctLegacy.Repos[0].Remotes, want)
	}

	// Name without leading slash (Docker sometimes omits it).
	noSlash := `[{"Name":"plain","State":{"Status":"running"},"Created":"2025-06-15T10:30:00Z","Config":{"Labels":{}}}]`
	ct2 := &Container{Client: testClient(t)}
	if err := ct2.fillFromInspect(t.Context(), []byte(noSlash)); err != nil {
		t.Fatalf("no-slash name: %v", err)
	}
	if ct2.Name != "plain" {
		t.Errorf("Name = %q, want %q", ct2.Name, "plain")
	}

	// Empty array.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`[]`)); err == nil {
		t.Error("expected error for empty array")
	}
	// Multiple results.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`[{},{}]`)); err == nil {
		t.Error("expected error for multiple results")
	}
	// Bad JSON.
	if err := (&Container{Client: testClient(t)}).fillFromInspect(t.Context(), []byte(`{bad}`)); err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestUSBRunArgs(t *testing.T) {
	t.Parallel()
	got := usbRunArgs([]string{"/dev/ttyACM0", "/dev/ttyUSB0"})
	want := []string{
		"-v", "/dev/bus/usb:/dev/bus/usb",
		"--device-cgroup-rule=c 189:* rwm",
		"--device=/dev/ttyACM0",
		"--device=/dev/ttyUSB0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("USB run arguments = %v, want %v", got, want)
	}
}

func TestParseCreatedAt(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			_, err := parseCreatedAt(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCreatedAt(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestTagRefspecs(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name   string
			regexp string
			tags   string
			want   []string
		}{
			{name: "empty regexp", tags: "v1\nv2"},
			{name: "selective", regexp: "^v", tags: "internal\nv1", want: []string{"refs/tags/v1:refs/tags/v1"}},
			{name: "all", regexp: ".*", tags: "internal\nv1", want: []string{"refs/tags/internal:refs/tags/internal", "refs/tags/v1:refs/tags/v1"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := tagRefspecs(tt.regexp, tt.tags)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("tagRefspecs(%q, %q) = %v, want %v", tt.regexp, tt.tags, got, tt.want)
				}
			})
		}
	})
	t.Run("large selection batches", func(t *testing.T) {
		t.Parallel()
		var tags strings.Builder
		for i := range 2000 {
			_, _ = fmt.Fprintf(&tags, "release-%04d-%s\n", i, strings.Repeat("x", 32))
		}
		tags.WriteString("internal\n")
		refspecs, err := tagRefspecs("^release-", strings.TrimSuffix(tags.String(), "\n"))
		if err != nil {
			t.Fatal(err)
		}
		batches := 0
		for start := 0; start < len(refspecs); {
			end := refspecBatchEnd(refspecs, start)
			if end <= start {
				t.Fatalf("batch did not advance from %d", start)
			}
			size := 0
			for _, refspec := range refspecs[start:end] {
				size += len(refspec) + 1
			}
			if size > maxPushRefspecBytes && end-start != 1 {
				t.Fatalf("batch size = %d, limit = %d", size, maxPushRefspecBytes)
			}
			batches++
			start = end
		}
		if batches < 2 {
			t.Fatalf("batches = %d, want multiple", batches)
		}
	})
}

func TestRepo(t *testing.T) {
	t.Parallel()
	t.Run("ResolveDefaults_tracked_default_branch_remote", func(t *testing.T) {
		t.Parallel()
		for _, branch := range []string{"main", "master"} {
			t.Run(branch, func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				dir := t.TempDir()
				originDir := filepath.Join(t.TempDir(), "origin.git")
				upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
				runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch="+branch, originDir)
				runTestGit(t, ctx, "", "init", "-q", "--bare", "--initial-branch="+branch, upstreamDir)
				runTestGit(t, ctx, dir, "init", "-q", "--initial-branch="+branch)
				runTestGit(t, ctx, dir, "config", "user.name", "Test")
				runTestGit(t, ctx, dir, "config", "user.email", "test@test")
				writeTestFile(t, filepath.Join(dir, "README.md"), branch+"\n")
				runTestGit(t, ctx, dir, "add", ".")
				runTestGit(t, ctx, dir, "commit", "-q", "-m", branch)
				runTestGit(t, ctx, dir, "remote", "add", "origin", originDir)
				runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
				runTestGit(t, ctx, dir, "push", "-q", "origin", branch)
				runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", branch)

				repo := Repo{GitRoot: dir, Branches: []string{branch}}
				if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
					t.Fatal(err)
				}
				if repo.DefaultRemote != "upstream" || repo.DefaultBranch != branch {
					t.Fatalf("defaults = %s/%s, want upstream/%s", repo.DefaultRemote, repo.DefaultBranch, branch)
				}
			})
		}
		t.Run("main_precedes_master", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "branch", "master")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")
			runTestGit(t, ctx, dir, "config", "branch.master.remote", "origin")
			runTestGit(t, ctx, dir, "config", "branch.master.merge", "refs/heads/master")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}}
			if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "upstream" {
				t.Fatalf("default remote = %q, want upstream", repo.DefaultRemote)
			}
		})
		t.Run("explicit_remote_is_authoritative", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}, DefaultRemote: "origin"}
			if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" || repo.DefaultBranch != "main" {
				t.Fatalf("defaults = %s/%s, want origin/main", repo.DefaultRemote, repo.DefaultBranch)
			}
		})
		t.Run("unmapped_upstream_is_ignored", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}, Remotes: []string{"origin"}}
			if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" {
				t.Fatalf("default remote = %q, want origin", repo.DefaultRemote)
			}
		})
		t.Run("local_and_synthetic_upstreams_are_ignored", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			cp := "/home/user/src/repo"
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "branch", "master")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "md-task", "user@md-task:"+cp)
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/md-task/master", "HEAD")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", ".")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/main")
			runTestGit(t, ctx, dir, "config", "branch.master.remote", "md-task")
			runTestGit(t, ctx, dir, "config", "branch.master.merge", "refs/heads/master")

			repo := Repo{
				GitRoot:       dir,
				Branches:      []string{"main", "master"},
				ContainerPath: cp,
			}
			if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "origin" {
				t.Fatalf("default remote = %q, want origin", repo.DefaultRemote)
			}
			if want := []string{"origin"}; !slices.Equal(repo.Remotes, want) {
				t.Fatalf("remotes = %v, want %v", repo.Remotes, want)
			}
			wantCommand := shellQuoteArgs([]string{"git", "-C", dir, "branch", "--set-upstream-to=origin/main", "master"})
			if _, err := repo.resolveContainerBranchBase(ctx, testLogger(t), "master"); err == nil || !strings.Contains(err.Error(), wantCommand) {
				t.Fatalf("resolveContainerBranchBase error = %v, want upstream repair command", err)
			}
		})
		t.Run("default_branch_comes_from_remote", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
			runTestGit(t, ctx, dir, "commit", "-q", "--allow-empty", "-m", "main")
			runTestGit(t, ctx, dir, "remote", "add", "origin", "/dev/null")
			runTestGit(t, ctx, dir, "remote", "add", "upstream", "/dev/null")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/release", "HEAD")
			runTestGit(t, ctx, dir, "update-ref", "refs/remotes/upstream/trunk", "HEAD")
			runTestGit(t, ctx, dir, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")
			runTestGit(t, ctx, dir, "config", "branch.main.remote", "upstream")
			runTestGit(t, ctx, dir, "config", "branch.main.merge", "refs/heads/release")

			repo := Repo{GitRoot: dir, Branches: []string{"main"}}
			if err := repo.ResolveDefaults(ctx, testLogger(t)); err != nil {
				t.Fatal(err)
			}
			if repo.DefaultRemote != "upstream" || repo.DefaultBranch != "trunk" {
				t.Fatalf("defaults = %s/%s, want upstream/trunk", repo.DefaultRemote, repo.DefaultBranch)
			}
		})
	})
	t.Run("ResolveDefaults_legacy_non_origin_remote", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
		runTestGit(t, ctx, "", "init", "-q", "--bare", upstreamDir)
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "config", "user.name", "Test")
		runTestGit(t, ctx, dir, "config", "user.email", "test@test")
		writeTestFile(t, filepath.Join(dir, "README.md"), "main\n")
		runTestGit(t, ctx, dir, "add", ".")
		runTestGit(t, ctx, dir, "commit", "-q", "-m", "main")
		runTestGit(t, ctx, dir, "remote", "add", "upstream", upstreamDir)
		runTestGit(t, ctx, dir, "push", "-q", "-u", "upstream", "main")
		cp := "/home/user/src/repo"
		runTestGit(t, ctx, dir, "remote", "add", "md-task-a", "user@md-task-a:"+cp)
		runTestGit(t, ctx, dir, "remote", "add", "md-task-b", "user@md-task-b:"+cp)

		logger := testLogger(t)
		ct := &Container{
			Logger: logger,
			Name:   "md-task-a",
			Repos:  []Repo{{GitRoot: dir, Branches: []string{"main"}, ContainerPath: cp}},
		}
		ct.migrateRepoRemotes(ctx)
		repo := &ct.Repos[0]
		if want := []string{"upstream"}; !slices.Equal(repo.Remotes, want) {
			t.Fatalf("migrated remotes = %v, want %v", repo.Remotes, want)
		}
		if err := repo.ResolveDefaults(ctx, logger); err != nil {
			t.Fatal(err)
		}
		if repo.DefaultRemote != "upstream" || repo.DefaultBranch != "main" {
			t.Fatalf("defaults = %s/%s, want upstream/main", repo.DefaultRemote, repo.DefaultBranch)
		}
	})
	t.Run("migrateRepoRemotes_removed_default", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		dir := t.TempDir()
		runTestGit(t, ctx, dir, "init", "-q", "--initial-branch=main")
		runTestGit(t, ctx, dir, "remote", "add", "backup", "/dev/null")
		repo := Repo{
			GitRoot:       dir,
			Branches:      []string{"main"},
			ContainerPath: "/home/user/src/repo",
			DefaultRemote: "upstream",
			DefaultBranch: "main",
		}
		ct := &Container{Logger: testLogger(t), Name: "md-task", Repos: []Repo{repo}}
		ct.migrateRepoRemotes(ctx)
		if want := []string{"backup", "upstream"}; !slices.Equal(ct.Repos[0].Remotes, want) {
			t.Fatalf("migrated remotes = %v, want %v", ct.Repos[0].Remotes, want)
		}
		if err := ct.Repos[0].Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("resolveMountPaths", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
				want string
			}{
				{
					"basename only",
					Repo{GitRoot: "/home/user/src/myrepo"},
					"/home/user/src/myrepo",
				},
				{
					"basename with .git suffix",
					Repo{GitRoot: "/home/user/src/myrepo.git"},
					"/home/user/src/myrepo",
				},
				{
					"ContainerPath overrides basename",
					Repo{GitRoot: "/home/user/src/myrepo", ContainerPath: "/home/user/src/custom-name"},
					"/home/user/src/custom-name",
				},
				{
					"ContainerPath preserves slashes",
					Repo{GitRoot: "/home/user/src/projects/foo/website", ContainerPath: "/home/user/src/foo/website"},
					"/home/user/src/foo/website",
				},
				{
					"empty ContainerPath falls back to basename",
					Repo{GitRoot: "/home/user/src/myrepo", ContainerPath: ""},
					"/home/user/src/myrepo",
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					repos := []Repo{tt.r}
					if err := resolveContainerPaths(repos); err != nil {
						t.Fatal(err)
					}
					if got := repos[0].ContainerPath; got != tt.want {
						t.Errorf("ContainerPath = %q, want %q", got, tt.want)
					}
				})
			}
		})
	})
	t.Run("Validate", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
			}{
				{"from basename", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}}},
				{"explicit absolute path", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "/home/user/src/custom"}},
				{"tilde expansion", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "~/src/custom"}},
				{"bare tilde", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "~"}},
				{"multiple remotes", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "upstream"}, DefaultRemote: "origin"}},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					if err := tt.r.Validate(); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name string
				r    Repo
				want string
			}{
				{"empty GitRoot", Repo{}, "GitRoot is empty"},
				{"empty Branches", Repo{GitRoot: "/home/user/src/myrepo"}, "Branches is empty"},
				{"empty branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", ""}}, "empty branch"},
				{"blank branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", "  "}}, "with whitespace"},
				{"duplicate branch", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main", "main"}}, "duplicate branch"},
				{"empty remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", ""}}, "empty remote"},
				{"blank remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "  "}}, "with whitespace"},
				{"duplicate remote", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"origin", "origin"}}, "duplicate remote"},
				{"default remote missing", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, Remotes: []string{"upstream"}, DefaultRemote: "origin"}, "is not in Repo.Remotes"},
				{"invalid tag regexp", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, TagRegexp: "["}, "invalid tag regexp"},
				{"relative ContainerPath", Repo{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}, ContainerPath: "custom"}, "must be an absolute POSIX path"},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					err := tt.r.Validate()
					if err == nil {
						t.Fatal("expected error, got nil")
					}
					if !strings.Contains(err.Error(), tt.want) {
						t.Errorf("error = %q, want containing %q", err.Error(), tt.want)
					}
				})
			}
		})
	})
}

func TestResolveMountPaths(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			repos []Repo
			want  []string
		}{
			{
				"single repo",
				[]Repo{{GitRoot: "/home/user/src/myrepo"}},
				[]string{"/home/user/src/myrepo"},
			},
			{
				"two repos with different basenames",
				[]Repo{
					{GitRoot: "/home/user/src/foo"},
					{GitRoot: "/home/user/src/bar"},
				},
				[]string{"/home/user/src/foo", "/home/user/src/bar"},
			},
			{
				"same basename but different ContainerPath",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website", ContainerPath: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/bar/website", ContainerPath: "/home/user/src/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
			{
				"same basename auto-disambiguated with relative path",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
			{
				"mixed explicit and auto-disambiguated",
				[]Repo{
					{GitRoot: "/home/user/src/foo/website"},
					{GitRoot: "/home/user/src/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/website"},
			},
			{
				"repos outside ~/src auto-disambiguate from common parent",
				[]Repo{
					{GitRoot: "/other/foo/website"},
					{GitRoot: "/other/bar/website"},
				},
				[]string{"/home/user/src/foo/website", "/home/user/src/bar/website"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if err := resolveContainerPaths(tt.repos); err != nil {
					t.Fatal(err)
				}
				for i, r := range tt.repos {
					if r.ContainerPath != tt.want[i] {
						t.Errorf("repos[%d].ContainerPath = %q, want %q", i, r.ContainerPath, tt.want[i])
					}
				}
			})
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("same repo different branches still conflicts", func(t *testing.T) {
			t.Parallel()
			repos := []Repo{
				{GitRoot: "/home/user/src/myrepo", Branches: []string{"main"}},
				{GitRoot: "/home/user/src/myrepo", Branches: []string{"feature"}},
			}
			err := resolveContainerPaths(repos)
			if err == nil {
				t.Fatal("expected error for same GitRoot after relative resolution")
			}
			if !strings.Contains(err.Error(), "both mount as") {
				t.Errorf("error should mention 'both mount as', got: %v", err)
			}
		})
	})
}

func TestParseDotenv(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		data := "\ufeff" + strings.Join([]string{
			"# comment",
			"",
			"ADB_MDNS_OPENSCREEN=0",
			"GEMINI_API_KEY = \"AIza secret\"",
			"GRADLE_OPTS = \"-Dorg.gradle.daemon=false\"",
			"OPENROUTER_API_KEY=sk-or-v1",
			"ASK_MODEL='qwen3-8-27b'",
			"export MODE=production",
			"WITH_COMMENT=value # note",
			"WITH_HASH=va#lue",
			"EMPTY=",
			`ESCAPED="line1\nline2"`,
			`EXPANDED="$HOME/bin:$PATH"`,
			"PLAIN=$HOME/bin",
			"LITERAL='$HOME/bin'",
		}, "\n")
		lines := parseDotenv([]byte(data))
		want := []string{
			"# comment",
			"ADB_MDNS_OPENSCREEN='0'",
			"GEMINI_API_KEY='AIza secret'",
			"GRADLE_OPTS='-Dorg.gradle.daemon=false'",
			"OPENROUTER_API_KEY='sk-or-v1'",
			"ASK_MODEL='qwen3-8-27b'",
			"MODE='production'",
			"WITH_COMMENT='value'",
			"WITH_HASH='va#lue'",
			"EMPTY=''",
			"ESCAPED='line1\nline2'",
			`EXPANDED="$HOME/bin:$PATH"`,
			"PLAIN=$HOME/bin",
			"LITERAL='$HOME/bin'",
		}
		if !slices.Equal(lines, want) {
			t.Errorf("parseDotenv() = %q, want %q", lines, want)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		data := strings.Join([]string{
			"NO_EQUALS",
			"1BAD=value",
			"=value",
			`UNTERMINATED="value`,
			"UNTERMINATED_SINGLE='value",
			`TRAILING="value" junk`,
		}, "\n")
		if lines := parseDotenv([]byte(data)); len(lines) != 0 {
			t.Errorf("parseDotenv() = %q, want none", lines)
		}
	})
}

func TestEnvFileContent(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, ".env"), []byte("A = \"1\"\nBAD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, ".env"), []byte("B=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := envFileContent([]string{first, second, empty}, []string{"C=c'c", "D="})
	if err != nil {
		t.Fatal(err)
	}
	want := "A='1'\nB='2'\nC='c'\\''c'\nunset D\n"
	if string(content) != want {
		t.Errorf("envFileContent() = %q, want %q", content, want)
	}

	missing, err := envFileContent([]string{empty}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Errorf("envFileContent() = %q, want no content", missing)
	}
}

// TestEnvFileContentSourced verifies the generated content is valid shell and
// that dotenv values survive sourcing, including the space-padded assignment
// that used to make the container print a shell diagnostic on every command.
func TestEnvFileContentSourced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := strings.Join([]string{
		"# comment",
		`GEMINI_API_KEY = "AIzaSyTEST"`,
		`A = "hello world"`,
		`B="it's"`,
		"EMPTY=",
		"WITH_COMMENT=value # note",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := envFileContent([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(t.TempDir(), "generated.env")
	if err := os.WriteFile(generated, content, 0o600); err != nil {
		t.Fatal(err)
	}
	script := ". " + shellQuote(generated) + ` && printf '%s\n%s\n%s\n%s' "$GEMINI_API_KEY" "$A" "$B" "$WITH_COMMENT"`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script) //nolint:gosec // script is generated from test literals.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sourcing generated .env: %v: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("sourcing generated .env printed %q, want nothing", stderr.String())
	}
	want := "AIzaSyTEST\nhello world\nit's\nvalue"
	if string(out) != want {
		t.Errorf("sourced variables = %q, want %q", out, want)
	}
}
