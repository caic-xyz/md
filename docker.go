// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:rsc
var rscFS embed.FS

// prepareBuildContext writes the embedded rsc/ tree to a temp directory.
// Returns the temp dir path (caller must clean up).
func prepareBuildContext() (dir string, retErr error) {
	tmp, err := os.MkdirTemp("", "md-build-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(tmp))
		}
	}()
	// Walk the embedded filesystem and write all files.
	err = fs.WalkDir(rscFS, "rsc", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Strip the leading "rsc/" prefix so we get a clean build context.
		rel := strings.TrimPrefix(path, "rsc/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := rscFS.ReadFile(path)
		if err != nil {
			return err
		}
		// Preserve executable bits for shell scripts.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, "xstartup") {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		return "", fmt.Errorf("extracting build context: %w", err)
	}
	return tmp, nil
}

// contextSHAHash computes a deterministic SHA-256 hash over the build context
// directory and the SSH key files. It walks files in sorted order and hashes
// each relative path and its contents.
func contextSHAHash(buildCtxDir, keysDir string) (string, error) {
	h := sha256.New()
	// Hash all files in buildCtxDir.
	if err := hashDir(h, buildCtxDir); err != nil {
		return "", err
	}
	// Hash specific key files from keysDir.
	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub", "authorized_keys"} {
		data, err := os.ReadFile(filepath.Join(keysDir, name))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, name)
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashDir walks dir in sorted order, hashing each file's relative path and
// contents into h.
func hashDir(h io.Writer, dir string) error {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, rel := range paths {
		// Use forward slashes so the hash matches embeddedContextSHA on all
		// platforms (fs.WalkDir on embed.FS always uses forward slashes).
		_, _ = io.WriteString(h, filepath.ToSlash(rel))
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		_, _ = h.Write(data)
	}
	return nil
}

func dockerInspectFormat(ctx context.Context, name, format string) (string, error) {
	return runCmd(ctx, "", []string{"docker", "image", "inspect", name, "--format", format}, true)
}

func getImageVersionLabel(ctx context.Context, imageName string) string {
	out, err := dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "org.opencontainers.image.version"}}`)
	if err != nil || out == "" || out == "<no value>" {
		return ""
	}
	return out
}

// getRemoteConfigDigest queries the registry for the config digest of the
// given image for the specified architecture without downloading layers.
func getRemoteConfigDigest(ctx context.Context, image, arch string) (string, error) {
	out, err := runCmd(ctx, "", []string{"docker", "manifest", "inspect", "-v", image}, true)
	if err != nil {
		return "", err
	}
	type configRef struct {
		Digest string `json:"digest"`
	}
	type manifestContent struct {
		Config configRef `json:"config"`
	}
	type manifestEntry struct {
		Descriptor struct {
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"Descriptor"`
		SchemaV2Manifest *manifestContent `json:"SchemaV2Manifest,omitempty"`
		OCIManifest      *manifestContent `json:"OCIManifest,omitempty"`
	}
	// Handle both v2 and OCI manifests.
	configDigest := func(e *manifestEntry) string {
		if e.SchemaV2Manifest != nil {
			return e.SchemaV2Manifest.Config.Digest
		}
		if e.OCIManifest != nil {
			return e.OCIManifest.Config.Digest
		}
		return ""
	}
	var entries []manifestEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		var single manifestEntry
		if err2 := json.Unmarshal([]byte(out), &single); err2 != nil {
			return "", fmt.Errorf("parsing manifest inspect output: %w", err)
		}
		entries = []manifestEntry{single}
	}
	for i := range entries {
		p := entries[i].Descriptor.Platform
		if p.Architecture == arch && p.OS == "linux" {
			if d := configDigest(&entries[i]); d != "" {
				return d, nil
			}
		}
	}
	if len(entries) == 1 {
		if d := configDigest(&entries[0]); d != "" {
			return d, nil
		}
	}
	return "", fmt.Errorf("no manifest for linux/%s in %s", arch, image)
}

// embeddedContextSHA computes a deterministic SHA-256 hash over the embedded
// build context (rsc/) and SSH key files without extracting to disk. It
// produces the same result as contextSHAHash on the extracted build context.
func embeddedContextSHA(keysDir string) (string, error) {
	h := sha256.New()
	var paths []string
	if err := fs.WalkDir(rscFS, "rsc", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if rel := strings.TrimPrefix(path, "rsc/"); rel != "" {
				paths = append(paths, rel)
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, rel := range paths {
		_, _ = io.WriteString(h, rel)
		data, err := rscFS.ReadFile("rsc/" + rel)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(data)
	}
	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub", "authorized_keys"} {
		data, err := os.ReadFile(filepath.Join(keysDir, name))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, name)
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cacheSpecKey returns a short hash over the requested cache names and
// container paths. Returns empty string when caches is nil or empty.
// Only the spec (name + path) is hashed, not the cache contents.
func cacheSpecKey(caches []CacheMount) string {
	if len(caches) == 0 {
		return ""
	}
	specs := make([]string, len(caches))
	for i, c := range caches {
		specs[i] = c.Name + ":" + c.ContainerPath
	}
	sort.Strings(specs)
	h := sha256.Sum256([]byte(strings.Join(specs, ",")))
	return hex.EncodeToString(h[:8])
}

// imageBuildNeeded reports whether the customized Docker image needs to be
// rebuilt. It checks the base image digest, build context SHA, and cache spec
// key against labels on the existing image. For remote base images it also
// verifies the local copy matches the registry.
// home is used to resolve "~/" in cache HostPaths so only caches whose host
// directory currently exists are compared (matching what appendCacheLayers
// would actually inject).
func imageBuildNeeded(ctx context.Context, imageName, baseImage, keysDir, home string, caches []CacheMount) bool {
	// Quick check: does the customized image have labels at all?
	currentDigest, err := dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.base_digest"}}`)
	if err != nil || currentDigest == "" || currentDigest == "<no value>" {
		return true
	}
	currentContext, err := dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.context_sha"}}`)
	if err != nil || currentContext == "" || currentContext == "<no value>" {
		return true
	}

	// Get the base image digest.
	var baseDigest string
	if d, err := dockerInspectFormat(ctx, baseImage, "{{index .RepoDigests 0}}"); err == nil && d != "" {
		baseDigest = d
	} else if id, err := dockerInspectFormat(ctx, baseImage, "{{.Id}}"); err == nil {
		baseDigest = id
	} else {
		return true
	}
	if currentDigest != baseDigest {
		return true
	}

	// For remote images, verify the local base is up to date with the registry.
	isLocal := !strings.Contains(baseImage, "/") && !strings.Contains(baseImage, ":")
	if !isLocal {
		localID, _ := dockerInspectFormat(ctx, baseImage, "{{.Id}}")
		remoteDigest, err := getRemoteConfigDigest(ctx, baseImage, runtime.GOARCH)
		if err != nil || remoteDigest != localID {
			return true
		}
	}

	// Compute context SHA from embedded FS without disk extraction.
	contextSHA, err := embeddedContextSHA(keysDir)
	if err != nil {
		return true
	}
	if currentContext != contextSHA {
		return true
	}

	// Compare what appendCacheLayers would actually inject (active caches whose
	// host dirs exist) against what's currently baked in. Using the active key
	// avoids perpetual rebuilds when some requested cache dirs don't exist yet.
	var activeCaches []CacheMount
	for _, c := range caches {
		if _, err := os.Stat(resolveHostPath(c.HostPath, home)); err == nil {
			activeCaches = append(activeCaches, c)
		}
	}
	currentKey, err := dockerInspectFormat(ctx, imageName, `{{index .Config.Labels "md.cache_key"}}`)
	if err != nil || currentKey == "<no value>" {
		currentKey = ""
	}
	if cacheSpecKey(activeCaches) != currentKey {
		return true
	}

	return false
}

// appendCacheLayers appends a RUN mkdir and COPY instructions to the Dockerfile
// at dockerfilePath, returns --build-context args for docker build and the
// cache spec key for the caches that were actually injected (may differ from
// the requested set when some host paths do not exist).
// mountPaths lists container-side -v mount targets whose leaf dirs must be
// pre-created. For caches only the intermediary ancestors are created; COPY
// --chown handles the leaf. Caches whose host path does not exist are silently
// skipped. "~/" in HostPath is resolved using home.
func appendCacheLayers(dockerfilePath string, caches []CacheMount, home string, mountPaths []string) (extraArgs []string, activeKey string, err error) {
	type activeCM struct {
		cm       CacheMount
		hostPath string
	}
	var active []activeCM
	for _, cm := range caches {
		hostPath := resolveHostPath(cm.HostPath, home)
		if _, err := os.Stat(hostPath); err != nil {
			continue
		}
		active = append(active, activeCM{cm, hostPath})
	}

	// activeKey reflects only the caches actually injected, not all requested.
	// This ensures imageBuildNeeded detects when a previously-skipped cache dir
	// appears on the host and triggers a rebuild.
	activeMounts := make([]CacheMount, len(active))
	for i, a := range active {
		activeMounts[i] = a.cm
	}
	activeKey = cacheSpecKey(activeMounts)

	// Collect directories to pre-create:
	// - For cache COPY destinations: intermediaries only; COPY --chown creates
	//   and owns the leaf.
	// - For runtime -v mount targets: the full path (leaf included), since no
	//   COPY will create it. mkdir -p also covers any intermediaries.
	const base = "/home/user"
	seen := map[string]bool{}
	for _, a := range active {
		for dir := path.Dir(a.cm.ContainerPath); dir != base && dir != "." && dir != "/"; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	for _, p := range mountPaths {
		seen[p] = true
	}

	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	extraArgs = make([]string, 0, 2*len(active))
	var copies strings.Builder
	for _, a := range active {
		fmt.Fprintf(&copies, "COPY --chown=user:user --from=%s / %s/\n", a.cm.Name, a.cm.ContainerPath)
		extraArgs = append(extraArgs, "--build-context", a.cm.Name+"="+a.hostPath)
	}

	if len(dirs) == 0 && copies.Len() == 0 {
		return nil, activeKey, nil
	}
	f, err := os.OpenFile(dockerfilePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, activeKey, err
	}
	var snippet strings.Builder
	snippet.WriteString("\n# Pre-create mount targets and inject build-time cache layers.\n")
	if len(dirs) > 0 {
		joined := strings.Join(dirs, " ")
		fmt.Fprintf(&snippet, "RUN mkdir -p %s && chown user:user %s\n", joined, joined)
	}
	snippet.WriteString(copies.String())
	_, err = fmt.Fprint(f, snippet.String())
	return extraArgs, activeKey, errors.Join(err, f.Close())
}

// buildCustomizedImage builds the per-user Docker image. keysDir is the
// directory containing SSH host keys and authorized_keys, supplied to Docker
// as a named build context "md-keys". home is the user's home directory used
// to resolve "~/" in cache HostPaths. mountPaths lists container-side -v mount
// targets to pre-create with user ownership.
func buildCustomizedImage(ctx context.Context, w io.Writer, buildCtxDir, keysDir, imageName, baseImage, home string, caches []CacheMount, mountPaths []string, quiet bool) error {
	arch := runtime.GOARCH
	// Local-only images (no "/" or ":" in name) are never pulled from a registry.
	isLocal := !strings.Contains(baseImage, "/") && !strings.Contains(baseImage, ":")
	if isLocal {
		if _, err := runCmd(ctx, "", []string{"docker", "image", "inspect", "--format", "{{.Id}}", baseImage}, true); err != nil {
			return fmt.Errorf("local image %s not found; build it first with 'md build-image'", baseImage)
		}
		if !quiet {
			_, _ = fmt.Fprintf(w, "- Using local base image %s.\n", baseImage)
		}
	} else {
		// Check if local image is already up to date with remote.
		needsPull := true
		if localID, err := runCmd(ctx, "", []string{"docker", "image", "inspect", "--format", "{{.Id}}", baseImage}, true); err == nil && localID != "" {
			if remoteDigest, err := getRemoteConfigDigest(ctx, baseImage, arch); err == nil && remoteDigest == localID {
				needsPull = false
			}
		}
		if needsPull {
			if !quiet {
				_, _ = fmt.Fprintf(w, "- Pulling base image %s ...\n", baseImage)
			}
			args := []string{"docker", "pull", "--platform", "linux/" + arch, baseImage}
			if _, err := runCmd(ctx, "", args, quiet); err != nil {
				return fmt.Errorf("pulling base image: %w", err)
			}
			if !quiet {
				if v := getImageVersionLabel(ctx, baseImage); strings.HasPrefix(v, "v") {
					_, _ = fmt.Fprintf(w, "  Version: %s\n", v)
				}
			}
		} else if !quiet {
			_, _ = fmt.Fprintf(w, "- Base image %s is up to date.\n", baseImage)
		}
	}

	// Get base image digest. For locally-built images (no registry), RepoDigests
	// is empty; fall back to the image ID so the label is never stored as "".
	baseDigest, err := runCmd(ctx, "", []string{"docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", baseImage}, true)
	if err != nil || baseDigest == "" {
		baseDigest, _ = runCmd(ctx, "", []string{"docker", "image", "inspect", "--format", "{{.Id}}", baseImage}, true)
	}

	// Compute context SHA from both the build context and keys directory.
	contextSHA, err := contextSHAHash(buildCtxDir, keysDir)
	if err != nil {
		return fmt.Errorf("computing context SHA: %w", err)
	}

	// Inject COPY layers into the extracted Dockerfile.
	// activeKey reflects only the caches that were actually found on the host;
	// use it (not cacheSpecKey(caches)) so the label accurately represents what
	// was baked in.
	var cacheArgs []string
	var activeKey string
	cacheArgs, activeKey, err = appendCacheLayers(filepath.Join(buildCtxDir, "Dockerfile"), caches, home, mountPaths)
	if err != nil {
		return fmt.Errorf("appending cache layers: %w", err)
	}

	if !quiet {
		_, _ = fmt.Fprintf(w, "- Building Docker image %s from %s ...\n", imageName, baseImage)
	}
	buildCmd := []string{
		"docker", "build",
		"--build-context", "md-keys=" + keysDir,
		"-f", filepath.Join(buildCtxDir, "Dockerfile"),
		"--platform", "linux/" + arch,
		"--build-arg", "BASE_IMAGE=" + baseImage,
		"--build-arg", "BASE_IMAGE_DIGEST=" + baseDigest,
		"--build-arg", "CONTEXT_SHA=" + contextSHA,
		"--build-arg", "CACHE_KEY=" + activeKey,
		"-t", imageName,
	}
	if quiet {
		buildCmd = append(buildCmd[:2], append([]string{"-q"}, buildCmd[2:]...)...)
	}
	buildCmd = append(buildCmd, cacheArgs...)
	buildCmd = append(buildCmd, buildCtxDir)
	if _, err := runCmd(ctx, "", buildCmd, false); err != nil {
		return fmt.Errorf("building Docker image: %w", err)
	}
	return nil
}

// dirStats returns the number of regular files and total byte size under dir.
// Unreadable entries are silently skipped.
func dirStats(dir string) (files, bytes int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				files++
				bytes += info.Size()
			}
		}
		return nil
	})
	return files, bytes
}

// formatBytes formats n bytes as a human-readable string (e.g. "1.2 GB").
func formatBytes(n int64) string {
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

// formatCount formats n with comma thousands separators (e.g. 1234567 → "1,234,567").
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := len(s) % 3
	var b strings.Builder
	b.Grow(len(s) + len(s)/3)
	if start > 0 {
		b.WriteString(s[:start])
	}
	for i := start; i < len(s); i += 3 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// resolveHostPath expands a leading "~/" to home; absolute paths are returned
// unchanged.
func resolveHostPath(p, home string) string {
	if strings.HasPrefix(p, "~/") {
		return path.Join(home, p[2:])
	}
	return p
}

// printCacheInfo walks each cache directory in parallel and writes one summary
// line per cache to w. "~/" in HostPath is resolved using home. Caches whose
// host path does not exist are reported as "not found".
func printCacheInfo(w io.Writer, caches []CacheMount, home string) {
	type stat struct {
		name     string
		hostPath string
		files    int64
		bytes    int64
		missing  bool
	}
	stats := make([]stat, len(caches))
	var wg sync.WaitGroup
	for i, c := range caches {
		wg.Add(1)
		go func(i int, c CacheMount) {
			defer wg.Done()
			hostPath := resolveHostPath(c.HostPath, home)
			stats[i].name = c.Name
			stats[i].hostPath = hostPath
			if _, err := os.Stat(hostPath); err != nil {
				stats[i].missing = true
				return
			}
			stats[i].files, stats[i].bytes = dirStats(hostPath)
		}(i, c)
	}
	wg.Wait()
	for _, s := range stats {
		if s.missing {
			_, _ = fmt.Fprintf(w, "- Cache %s: %s not found, skipping\n", s.name, s.hostPath)
			continue
		}
		_, _ = fmt.Fprintf(w, "- Cache %s (%s): %s files, %s\n",
			s.name, s.hostPath, formatCount(s.files), formatBytes(s.bytes))
	}
}

// runContainer starts the Docker container and sets up SSH access.
// tailscaleEphemeral indicates the Tailscale node was auto-keyed and should be
// treated as ephemeral (cleaned up on kill without API deletion).
func runContainer(ctx context.Context, c *Container, opts *StartOpts, tailscaleEphemeral bool) (*StartResult, error) {
	var dockerArgs []string
	dockerArgs = append(dockerArgs, "docker", "run", "-d",
		"--name", c.Name, "--hostname", c.Name,
		"-p", "127.0.0.1:0:22")

	if opts.Display {
		dockerArgs = append(dockerArgs, "-p", "127.0.0.1:0:5901", "-e", "MD_DISPLAY=1")
	}

	if kvmAvailable() {
		dockerArgs = append(dockerArgs, "--device=/dev/kvm")
	}
	// Localtime.
	if runtime.GOOS == "linux" {
		dockerArgs = append(dockerArgs, "-v", "/etc/localtime:/etc/localtime:ro")
	}
	// Sandbox capabilities.
	dockerArgs = append(dockerArgs,
		"--cap-add=SYS_PTRACE",
		"--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined")

	// Tailscale.
	if opts.Tailscale {
		dockerArgs = append(dockerArgs,
			"--cap-add=NET_ADMIN", "--cap-add=NET_RAW", "--cap-add=MKNOD",
			"-e", "MD_TAILSCALE=1")
		if opts.TailscaleAuthKey != "" {
			dockerArgs = append(dockerArgs, "-e", "TAILSCALE_AUTHKEY="+opts.TailscaleAuthKey)
		}
		if tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "-e", "MD_TAILSCALE_EPHEMERAL=1")
		}
	}

	// USB passthrough (Linux only; Docker Desktop on macOS/Windows runs in a
	// VM that cannot access host USB devices). Use a bind mount + cgroup
	// rule so that devices plugged in after container start are visible.
	if opts.USB {
		if runtime.GOOS != "linux" {
			return nil, fmt.Errorf("--usb requires Linux; Docker Desktop on %s cannot pass through host USB devices", runtime.GOOS)
		}
		dockerArgs = append(dockerArgs,
			"-v", "/dev/bus/usb:/dev/bus/usb",
			"--device-cgroup-rule=c 189:* rwm")
	}

	// Agent config mounts.
	home := c.Home
	xdgConfig := c.XDGConfigHome
	xdgData := c.XDGDataHome
	xdgState := c.XDGStateHome
	for _, p := range agentConfig.HomePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(home, p)+":/home/user/"+p)
	}
	for _, p := range agentConfig.XDGConfigPaths {
		ro := ""
		if p == "md" {
			ro = ":ro"
		}
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgConfig, p)+":/home/user/.config/"+p+ro)
	}
	for _, p := range agentConfig.LocalSharePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgData, p)+":/home/user/.local/share/"+p)
	}
	for _, p := range agentConfig.LocalStatePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgState, p)+":/home/user/.local/state/"+p)
	}

	// Set md metadata labels.
	// TODO: This will need a bit more work when we support multiple repos.
	dockerArgs = append(dockerArgs,
		"--label", "md.git_root="+c.GitRoot,
		"--label", "md.repo_name="+c.RepoName,
		"--label", "md.branch="+c.Branch)
	if opts.Display {
		dockerArgs = append(dockerArgs, "--label", "md.display=1")
	}
	if opts.Tailscale {
		dockerArgs = append(dockerArgs, "--label", "md.tailscale=1")
		if tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "--label", "md.tailscale_ephemeral=1")
		}
	}
	if opts.USB {
		dockerArgs = append(dockerArgs, "--label", "md.usb=1")
	}
	for _, l := range opts.Labels {
		dockerArgs = append(dockerArgs, "--label", l)
	}
	dockerArgs = append(dockerArgs, c.ImageName)

	if opts.Quiet {
		if _, err := runCmd(ctx, "", dockerArgs, true); err != nil {
			return nil, fmt.Errorf("starting container: %w", err)
		}
	} else {
		_, _ = fmt.Fprintf(c.W, "- Starting container %s ... ", c.Name)
		if _, err := runCmd(ctx, "", dockerArgs, false); err != nil {
			_, _ = fmt.Fprintln(c.W)
			return nil, fmt.Errorf("starting container: %w", err)
		}
	}

	result := &StartResult{}

	// Get SSH port and creation time.
	port, err := runCmd(ctx, "", []string{"docker", "inspect", "--format", `{{(index .NetworkSettings.Ports "22/tcp" 0).HostPort}}`, c.Name}, true)
	if err != nil {
		return nil, fmt.Errorf("getting SSH port: %w", err)
	}
	result.SSHPort = port
	if !opts.Quiet {
		_, _ = fmt.Fprintf(c.W, "- Found ssh port %s\n", port)
	}
	createdStr, err := runCmd(ctx, "", []string{"docker", "inspect", "--format", "{{.Created}}", c.Name}, true)
	if err != nil {
		return nil, fmt.Errorf("getting container creation time: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdStr)
	if err != nil {
		return nil, fmt.Errorf("parsing container creation time %q: %w", createdStr, err)
	}
	c.CreatedAt = created

	// Get VNC port if display enabled.
	if opts.Display {
		vncPort, _ := runCmd(ctx, "", []string{"docker", "inspect", "--format", `{{(index .NetworkSettings.Ports "5901/tcp" 0).HostPort}}`, c.Name}, true)
		result.VNCPort = vncPort
		if vncPort != "" && !opts.Quiet {
			_, _ = fmt.Fprintf(c.W, "- Found VNC port %s (display :1)\n", vncPort)
		}
	}

	// Write SSH config.
	sshConfigDir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
		return nil, err
	}
	knownHostsPath := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	hostPubKey, err := os.ReadFile(c.HostKeyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("reading host public key: %w", err)
	}
	if err := writeSSHConfig(sshConfigDir, c.Name, port, c.UserKeyPath, knownHostsPath); err != nil {
		return nil, err
	}
	if err := writeKnownHosts(knownHostsPath, port, strings.TrimSpace(string(hostPubKey))); err != nil {
		return nil, err
	}

	// Set up git remote.
	if !opts.Quiet {
		_, _ = fmt.Fprintln(c.W, "- git clone into container ...")
	}
	_, _ = runCmd(ctx, c.GitRoot, []string{"git", "remote", "rm", c.Name}, true)
	if _, err := runCmd(ctx, c.GitRoot, []string{"git", "remote", "add", c.Name, "user@" + c.Name + ":/home/user/src/" + c.RepoName}, false); err != nil {
		return nil, fmt.Errorf("adding git remote: %w", err)
	}

	// Wait for SSH.
	start := time.Now()
	var lastOutput string
	for {
		out, err := runCmd(ctx, "", []string{"ssh", "-o", "ConnectTimeout=2", c.Name, "exit"}, true)
		if err == nil {
			break
		}
		lastOutput = out
		time.Sleep(100 * time.Millisecond)
		if time.Since(start) > 10*time.Second {
			return nil, fmt.Errorf("timed out waiting for container SSH: %s", lastOutput)
		}
	}

	repo := shellQuote(c.RepoName)
	branch := shellQuote(c.Branch)

	// Initialize git repo in container.
	if _, err := runCmd(ctx, "", []string{"ssh", c.Name, "git init -q ~/src/" + repo}, false); err != nil {
		return nil, err
	}
	if _, err := runCmd(ctx, c.GitRoot, []string{"git", "push", "-q", "--tags", c.Name, c.Branch + ":refs/heads/" + c.Branch}, false); err != nil {
		return nil, err
	}
	if _, err := runCmd(ctx, "", []string{"ssh", c.Name, "cd ~/src/" + repo + " && git switch -q " + branch + " && git branch -f base " + branch + " && git switch -q base && git switch -q " + branch}, false); err != nil {
		return nil, err
	}

	// Set up origin remote in container using HTTPS.
	if err := c.resolveDefaults(ctx); err != nil {
		return nil, err
	}
	originURL, err := runCmd(ctx, c.GitRoot, []string{"git", "remote", "get-url", c.DefaultRemote}, true)
	if err == nil && originURL != "" {
		httpsURL := convertGitURLToHTTPS(originURL)
		_, _ = runCmd(ctx, "", []string{"ssh", c.Name, "cd ~/src/" + repo + " && git remote add origin " + shellQuote(httpsURL)}, true)
		if !opts.Quiet {
			_, _ = fmt.Fprintf(c.W, "- Set container origin to %s\n", httpsURL)
		}
	}

	// Push the default branch (e.g. main) so agents can diff against it.
	if err := c.SyncDefaultBranch(ctx); err != nil {
		return nil, err
	}

	// Copy .env if present.
	if _, err := os.Stat(".env"); err == nil {
		if !opts.Quiet {
			_, _ = fmt.Fprintln(c.W, "- sending .env into container ...")
		}
		_, _ = runCmd(ctx, "", []string{"scp", ".env", c.Name + ":/home/user/.env"}, false)
	}

	// Wait for Tailscale auth URL if needed.
	if opts.Tailscale && opts.TailscaleAuthKey == "" {
		cmd := exec.CommandContext(ctx, "ssh", c.Name, "tail -f /tmp/tailscale_auth_url")
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			if err := cmd.Start(); err == nil {
				buf := make([]byte, 4096)
				n, _ := stdout.Read(buf)
				line := string(buf[:n])
				if strings.Contains(line, "https://") {
					result.TailscaleAuthURL = strings.TrimSpace(line)
				}
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
	}

	return result, nil
}

// convertGitURLToHTTPS converts a git URL to HTTPS format.
//
// Supports git@host:path, ssh://git@host/path, git://host/path, and
// https:// (returned unchanged).
func convertGitURLToHTTPS(url string) string {
	url = strings.TrimSpace(url)
	if strings.HasPrefix(url, "https://") {
		return url
	}
	// Matches git@host:user/repo.git
	if m := reGitAt.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	// Matches ssh://git@host/user/repo.git
	if m := reSSHGit.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	// Matches git://host/user/repo.git
	if m := reGitProto.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("https://%s/%s", m[1], m[2])
	}
	return url
}
