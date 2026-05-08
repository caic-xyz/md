// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// DefaultMaxCPUs returns max(2, NumCPU-2), a sensible CPU limit that
// leaves headroom for the host while guaranteeing at least 2 cores.
func DefaultMaxCPUs() int {
	return max(2, runtime.NumCPU()-2)
}

//go:embed all:rsc
var rscFS embed.FS

// extractEmbeddedTree writes an embedded rsc/ subtree to a temp directory.
//
// prefix is the embedded path (e.g. "rsc/user"), tmpPattern is the os.MkdirTemp
// pattern. Returns the temp dir path (caller must clean up).
func extractEmbeddedTree(prefix, tmpPattern string) (dir string, retErr error) {
	tmp, err := os.MkdirTemp("", tmpPattern)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(tmp))
		}
	}()
	err = fs.WalkDir(rscFS, prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, prefix+"/")
		if rel == "" || rel == path {
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
		// Preserve executable bits for scripts (by extension, shebang, or bin-directory location).
		mode := os.FileMode(0o644)
		if isExecutable(data) {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		return "", fmt.Errorf("extracting %s: %w", prefix, err)
	}
	return tmp, nil
}

// isExecutable reports whether a file from the embedded rsc filesystem should
// be written with execute permission. Matches files starting with a #! shebang.
func isExecutable(data []byte) bool {
	return bytes.HasPrefix(data, []byte("#!"))
}

// prepareBuildContext writes the embedded rsc/user/ tree to a temp directory.
//
// Returns the temp dir path (caller must clean up).
func prepareBuildContext() (string, error) {
	return extractEmbeddedTree("rsc/user", "md-build-*")
}

// prepareRootBuildContext writes the embedded rsc/root/ tree to a temp
// directory.
//
// Returns the temp dir path (caller must clean up).
func prepareRootBuildContext() (string, error) {
	return extractEmbeddedTree("rsc/root", "md-build-root-*")
}

// keysSHA computes a deterministic SHA-256 hash over the SSH key files in
// keysDir. This is used to detect when SSH keys change and trigger an image
// rebuild.
func keysSHA(keysDir string) (string, error) {
	h := sha256.New()
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

func dockerInspectFormat(ctx context.Context, rt, name, format string) (string, error) {
	return runCmd(ctx, "", []string{rt, "image", "inspect", name, "--format", format})
}

func getImageVersionLabel(ctx context.Context, rt, imageName string) string {
	out, err := dockerInspectFormat(ctx, rt, imageName, `{{index .Config.Labels "org.opencontainers.image.version"}}`)
	if err != nil || out == "" || out == "<no value>" {
		return ""
	}
	return out
}

// getRemoteManifestDigest queries the registry for the per-architecture
// manifest digest without downloading layers.
//
// For a multi-arch image the digest hierarchy is:
//
//	Image Index (manifest list)         sha256:AAA
//	  └── Per-platform Manifest (amd64) sha256:BBB  ← manifest digest
//	        ├── Config                  sha256:CCC  ← docker's {{.Id}}
//	        └── Layers ...
//
// We compare manifest digests (sha256:BBB): this is what "docker pull" prints,
// what {{index .RepoDigests 0}} stores as "repo@sha256:BBB", and what
// "manifest inspect" returns in manifests[].digest. Any change to layers,
// config, or manifest metadata produces a different manifest digest, making it
// a reliable staleness signal.
//
// Both Docker schema v2 manifest lists and OCI image indexes share the same
// "manifests[].{digest, platform}" JSON structure, so one parser covers both
// runtimes and both formats.
func getRemoteManifestDigest(ctx context.Context, rt, image, arch string) (string, error) {
	slog.DebugContext(ctx, "md", "msg", "fetching remote manifest digest", "image", image, "arch", arch)
	out, err := runCmd(ctx, "", []string{rt, "manifest", "inspect", image})
	if err != nil {
		return "", err
	}
	var index manifestIndex
	if err := json.Unmarshal([]byte(out), &index); err != nil {
		return "", fmt.Errorf("parsing manifest inspect output: %w", err)
	}
	for _, m := range index.Manifests {
		if m.Platform.Architecture == arch && m.Platform.OS == "linux" && m.Digest != "" {
			return m.Digest, nil
		}
	}
	if len(index.Manifests) == 1 && index.Manifests[0].Digest != "" {
		return index.Manifests[0].Digest, nil
	}
	return "", fmt.Errorf("no manifest for linux/%s in %s", arch, image)
}

type remoteDigestEntry struct {
	digest  string
	err     error
	expires time.Time
}

type manifestPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type manifestEntry struct {
	Digest   string           `json:"digest"`
	Platform manifestPlatform `json:"platform"`
}

type manifestIndex struct {
	Manifests []manifestEntry `json:"manifests"`
}

type activeCM struct {
	cm       CacheMount
	hostPath string
	// files lists top-level filenames for Shallow caches. nil for recursive.
	files []string
}

// imageBuildCacheEntry caches the result of imageBuildNeeded so that
// back-to-back calls with the same inputs skip docker inspect exec calls.
type imageBuildCacheEntry struct {
	baseImage  string
	contextSHA string
	cacheKey   string
	needed     bool
}

// cachedRemoteManifestDigest returns the remote per-architecture manifest digest.
// When Client.DigestCacheTTL is non-zero, results are cached for that duration
// to skip repeated registry round-trips. When zero, the registry is always queried.
func (c *Client) cachedRemoteManifestDigest(ctx context.Context, rt, image, arch string) (string, error) {
	if c.DigestCacheTTL == 0 {
		return getRemoteManifestDigest(ctx, rt, image, arch)
	}
	key := rt + "\x00" + image + "\x00" + arch
	c.mu.Lock()
	if e, ok := c.digestCache[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.digest, e.err
	}
	c.mu.Unlock()
	digest, err := getRemoteManifestDigest(ctx, rt, image, arch)
	c.mu.Lock()
	c.digestCache[key] = remoteDigestEntry{digest: digest, err: err, expires: time.Now().Add(c.DigestCacheTTL)}
	c.mu.Unlock()
	return digest, err
}

// activeCacheKey filters caches to those whose host directories exist and
// returns the cache spec key for the active set.
func activeCacheKey(caches []CacheMount, home string) string {
	var active []CacheMount
	for _, cm := range caches {
		if _, err := os.Stat(resolveHostPath(cm.HostPath, home)); err == nil {
			active = append(active, cm)
		}
	}
	return cacheSpecKey(active)
}

// userImageName returns the Docker image name for a given base image and
// active cache configuration. The name includes a content hash so that
// different base images or cache sets produce distinct images without
// clobbering each other.
func userImageName(baseImage, cacheKey string) string {
	h := sha256.Sum256([]byte(baseImage + "\x00" + cacheKey))
	return "md-specialized-" + hex.EncodeToString(h[:16])
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
		s := c.Name + ":" + c.ContainerPath
		if c.Shallow {
			s += ":shallow"
		}
		specs[i] = s
	}
	sort.Strings(specs)
	h := sha256.Sum256([]byte(strings.Join(specs, ",")))
	return hex.EncodeToString(h[:8])
}

// imageBuildNeeded reports whether the specialized Docker image needs to be
// rebuilt. It checks the base image digest, SSH keys hash, and cache spec
// key against labels on the existing image. For remote base images it also
// verifies the local copy matches the registry.
// home is used to resolve "~/" in cache HostPaths so only caches whose host
// directory currently exists are compared (matching what resolveCaches
// would actually inject).
func (c *Client) imageBuildNeeded(ctx context.Context, rt, imageName, baseImage, keysDir, home string, caches []CacheMount) bool {
	// Compute cheap inputs first so we can check the cache.
	contextSHA, err := keysSHA(keysDir)
	if err != nil {
		return true
	}
	var activeCaches []CacheMount
	for _, cm := range caches {
		if _, err := os.Stat(resolveHostPath(cm.HostPath, home)); err == nil {
			activeCaches = append(activeCaches, cm)
		}
	}
	activeKey := cacheSpecKey(activeCaches)

	// Check cached result from a previous call with the same inputs.
	c.mu.Lock()
	if e := c.imageBuildCache; e != nil && e.baseImage == baseImage && e.contextSHA == contextSHA && e.cacheKey == activeKey {
		needed := e.needed
		c.mu.Unlock()
		return needed
	}
	c.mu.Unlock()

	needed := c.imageBuildNeededSlow(ctx, rt, imageName, baseImage, contextSHA, activeKey)

	c.mu.Lock()
	c.imageBuildCache = &imageBuildCacheEntry{
		baseImage:  baseImage,
		contextSHA: contextSHA,
		cacheKey:   activeKey,
		needed:     needed,
	}
	c.mu.Unlock()
	return needed
}

// invalidateImageBuildCache clears the cached imageBuildNeeded result.
// Must be called after a successful image build so the next check re-evaluates.
func (c *Client) invalidateImageBuildCache() {
	c.mu.Lock()
	c.imageBuildCache = nil
	c.mu.Unlock()
}

// imageBuildNeededSlow performs the full check with docker inspect calls.
func (c *Client) imageBuildNeededSlow(ctx context.Context, rt, imageName, baseImage, contextSHA, activeKey string) bool {
	slog.DebugContext(ctx, "md", "msg", "checking if image build needed", "image", imageName, "base", baseImage)
	// Quick check: does the specialized image have labels at all?
	currentDigest, err := dockerInspectFormat(ctx, rt, imageName, `{{index .Config.Labels "md.base_digest"}}`)
	if err != nil || currentDigest == "" || currentDigest == "<no value>" {
		slog.DebugContext(ctx, "md", "msg", "build needed: no base_digest label", "image", imageName)
		return true
	}
	currentContext, err := dockerInspectFormat(ctx, rt, imageName, `{{index .Config.Labels "md.context_sha"}}`)
	if err != nil || currentContext == "" || currentContext == "<no value>" {
		slog.DebugContext(ctx, "md", "msg", "build needed: no context_sha label", "image", imageName)
		return true
	}

	// Get the base image digest.
	var baseDigest string
	if d, err := dockerInspectFormat(ctx, rt, baseImage, "{{index .RepoDigests 0}}"); err == nil && d != "" {
		baseDigest = d
	} else if id, err := dockerInspectFormat(ctx, rt, baseImage, "{{.Id}}"); err == nil {
		baseDigest = id
	} else {
		slog.DebugContext(ctx, "md", "msg", "build needed: cannot get base image digest", "base", baseImage)
		return true
	}
	if currentDigest != baseDigest {
		slog.DebugContext(ctx, "md", "msg", "build needed: base digest changed", "current", currentDigest, "base", baseDigest)
		return true
	}

	// For remote images, verify the local base is up to date with the registry.
	// Compare the per-platform manifest digest stored during the last build
	// against the current remote per-platform digest. This avoids the
	// manifest-list-vs-platform-manifest mismatch that occurs when comparing
	// RepoDigests[0] (manifest list digest) against the per-platform entry.
	// Errors are intentionally ignored: a registry failure is not a reason to rebuild;
	// the base digest label comparison above already catches locally-pulled updates.
	isLocal := !strings.Contains(baseImage, "/")
	if !isLocal {
		slog.DebugContext(ctx, "md", "msg", "checking remote manifest digest", "base", baseImage)
		storedManifest, err := dockerInspectFormat(ctx, rt, imageName, `{{index .Config.Labels "md.base_manifest_digest"}}`)
		if err == nil && storedManifest != "" && storedManifest != "<no value>" {
			remoteDigest, err := c.cachedRemoteManifestDigest(ctx, rt, baseImage, runtime.GOARCH)
			if err == nil && remoteDigest != storedManifest {
				slog.DebugContext(ctx, "md", "msg", "build needed: remote manifest changed", "stored", storedManifest, "remote", remoteDigest)
				return true
			}
		}
	}

	if currentContext != contextSHA {
		slog.DebugContext(ctx, "md", "msg", "build needed: context SHA changed", "current", currentContext, "expected", contextSHA)
		return true
	}

	currentKey, err := dockerInspectFormat(ctx, rt, imageName, `{{index .Config.Labels "md.cache_key"}}`)
	if err != nil || currentKey == "<no value>" {
		currentKey = ""
	}
	if activeKey != currentKey {
		slog.DebugContext(ctx, "md", "msg", "build needed: cache key changed", "current", currentKey, "expected", activeKey)
		return true
	}

	slog.DebugContext(ctx, "md", "msg", "image is up to date", "image", imageName)
	return false
}

// resolveCaches determines which caches have existing host directories and
// computes the set of container directories that need to be pre-created.
// Returns the active caches (with resolved host paths), directories to
// pre-create, and the cache spec key. Caches whose host path does not exist
// are silently skipped.
func resolveCaches(caches []CacheMount, home string, mountPaths []string) (active []activeCM, dirs []string, activeKey string) {
	for _, cm := range caches {
		hostPath := resolveHostPath(cm.HostPath, home)
		if _, err := os.Stat(hostPath); err != nil {
			continue
		}
		a := activeCM{cm: cm, hostPath: hostPath}
		if cm.Shallow {
			entries, err := os.ReadDir(hostPath)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					a.files = append(a.files, e.Name())
				}
			}
			if len(a.files) == 0 {
				continue
			}
		}
		active = append(active, a)
	}

	// activeKey reflects only the caches actually injected, not all requested.
	activeMounts := make([]CacheMount, len(active))
	for i, a := range active {
		activeMounts[i] = a.cm
	}
	activeKey = cacheSpecKey(activeMounts)

	// Collect directories to pre-create:
	// - For cache destinations: intermediaries and the leaf itself.
	// - For runtime -v mount targets: the full path (leaf included).
	const base = "/home/user"
	seen := map[string]bool{}
	for _, a := range active {
		seen[a.cm.ContainerPath] = true
		for dir := path.Dir(a.cm.ContainerPath); dir != base && dir != "." && dir != "/"; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	for _, p := range mountPaths {
		seen[p] = true
	}
	dirs = make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return active, dirs, activeKey
}

// generateDockerfile produces the Dockerfile content for a specialized image.
func generateDockerfile(baseImage string, active []activeCM, dirs []string, baseDigest, contextSHA, activeKey, manifestDigest string) string {
	var df strings.Builder
	fmt.Fprintf(&df, "FROM %s\n", baseImage)
	df.WriteString("COPY --chown=root:root ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key\n")
	df.WriteString("COPY --chown=root:root ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ed25519_key.pub\n")
	df.WriteString("COPY --chown=user:user authorized_keys /home/user/.ssh/authorized_keys\n")
	for _, a := range active {
		if a.files != nil {
			// Shallow: copy only top-level files, skip subdirectories.
			// Flags must appear before the JSON array; the array contains only
			// sources and destination.
			for _, f := range a.files {
				fmt.Fprintf(&df, "COPY --from=cache-%s --chown=user:user [%q, %q]\n", a.cm.Name, f, a.cm.ContainerPath+"/")
			}
		} else {
			fmt.Fprintf(&df, "COPY --from=cache-%s --chown=user:user [\".\", %q]\n", a.cm.Name, a.cm.ContainerPath+"/")
		}
	}
	// Single RUN layer for file permissions and directory pre-creation.
	var run strings.Builder
	run.WriteString("chmod 0600 /etc/ssh/ssh_host_ed25519_key")
	run.WriteString(" && chmod 0644 /etc/ssh/ssh_host_ed25519_key.pub")
	run.WriteString(" && chmod 0400 /home/user/.ssh/authorized_keys")
	if len(dirs) > 0 {
		quoted := make([]string, len(dirs))
		for i, d := range dirs {
			quoted[i] = shellQuote(d)
		}
		joined := strings.Join(quoted, " ")
		fmt.Fprintf(&run, " && mkdir -p %s && chown user:user %s", joined, joined)
	}
	fmt.Fprintf(&df, "RUN %s\n", run.String())
	fmt.Fprintf(&df, "LABEL md.base_image=%q\n", baseImage)
	fmt.Fprintf(&df, "LABEL md.base_digest=%q\n", baseDigest)
	fmt.Fprintf(&df, "LABEL md.context_sha=%q\n", contextSHA)
	fmt.Fprintf(&df, "LABEL md.cache_key=%q\n", activeKey)
	fmt.Fprintf(&df, "LABEL md.base_manifest_digest=%q\n", manifestDigest)
	df.WriteString("CMD [\"/root/start.sh\"]\n")
	return df.String()
}

// buildSpecializedImage builds the per-user Docker image by generating a
// Dockerfile at build time and running "docker build".
//
// Design rationale — three approaches were evaluated:
//
//  1. docker create + docker cp + docker exec + docker commit: avoids BuildKit
//     entirely but "docker cp" is significantly slower than Dockerfile COPY
//     (API round-trips vs storage-driver-level tar streaming). Also requires
//     starting the container to fix file ownership, adding latency.
//
//  2. Maintained Dockerfile in rsc/specialized/ with BuildKit: fast COPY but
//     requires keeping the file in sync with runtime logic (which caches
//     exist, what mount paths to create). BuildKit's persistent build cache
//     also accumulates multi-GB of intermediate layers over repeated rebuilds,
//     requiring periodic "docker builder prune" and slowing subsequent builds
//     as the cache grows.
//
//  3. Maintained Dockerfile in rsc/specialized/ without BuildKit: avoids
//     BuildKit cache growth but cannot adapt to dynamic inputs and still
//     requires keeping the file in sync with runtime logic.
//
//  4. Generated Dockerfile + docker build (current): combines COPY's speed with
//     dynamic generation. Uses --build-context per cache directory so large host
//     trees are read in-place without copying into the build context. COPY
//     --chown sets ownership at copy time, eliminating the container
//     start/exec/stop cycle. --no-cache prevents stale layer reuse and keeps
//     BuildKit's residual cache small.
//
// keysDir contains SSH host keys and authorized_keys. home resolves "~/" in
// cache HostPaths. mountPaths lists container-side -v mount targets to
// pre-create with user ownership.
func buildSpecializedImage(ctx context.Context, stdout, stderr io.Writer, rt, keysDir, imageName, baseImage, home string, caches []CacheMount, mountPaths []string, quiet bool) error {
	slog.DebugContext(ctx, "md", "msg", "building specialized image", "image", imageName, "base", baseImage)
	arch := runtime.GOARCH
	// Local-only images (no "/" in name) are never pulled from a registry.
	// A tag (":latest") does not imply a registry; only a "/" does.
	isLocal := !strings.Contains(baseImage, "/")
	if isLocal {
		if _, err := runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{.Id}}", baseImage}); err != nil {
			return fmt.Errorf("local image %s not found; build it first with 'md build-image'", baseImage)
		}
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Using local base image %s.\n", baseImage)
		}
	} else {
		// Compare the local image ID before and after pull to detect changes.
		idBefore, _ := runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{.Id}}", baseImage})
		if !quiet {
			_, _ = fmt.Fprintf(stdout, "- Pulling base image %s ...\n", baseImage)
		}
		if quiet {
			if _, err := runCmd(ctx, "", []string{rt, "pull", "--platform", "linux/" + arch, baseImage}); err != nil {
				return cmdErrWithStderr("pulling base image", err)
			}
		} else {
			if err := runCmdOut(ctx, "", []string{rt, "pull", "--platform", "linux/" + arch, baseImage}, stdout, stderr); err != nil {
				return fmt.Errorf("pulling base image: %w", err)
			}
		}
		idAfter, _ := runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{.Id}}", baseImage})
		if !quiet {
			if idBefore != "" && idBefore == idAfter {
				_, _ = fmt.Fprintf(stdout, "  Base image is up to date.\n")
			} else if v := getImageVersionLabel(ctx, rt, baseImage); strings.HasPrefix(v, "v") {
				_, _ = fmt.Fprintf(stdout, "  Version: %s\n", v)
			}
		}
	}

	slog.DebugContext(ctx, "md", "msg", "pull complete, fetching base image digest")
	// Get base image digest for label.
	baseDigest, err := runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{index .RepoDigests 0}}", baseImage})
	if err != nil || baseDigest == "" {
		baseDigest, _ = runCmd(ctx, "", []string{rt, "image", "inspect", "--format", "{{.Id}}", baseImage})
	}
	var manifestDigest string
	if !isLocal {
		manifestDigest, _ = getRemoteManifestDigest(ctx, rt, baseImage, arch)
	}

	contextSHA, err := keysSHA(keysDir)
	if err != nil {
		return fmt.Errorf("computing keys SHA: %w", err)
	}

	active, dirs, activeKey := resolveCaches(caches, home, mountPaths)

	if !quiet {
		_, _ = fmt.Fprintf(stdout, "- Building container image %s from %s ...\n", imageName, baseImage)
		// Report skipped caches (host dir does not exist).
		activeNames := make(map[string]bool, len(active))
		for _, a := range active {
			activeNames[a.cm.Name] = true
		}
		for _, cm := range caches {
			if !activeNames[cm.Name] {
				_, _ = fmt.Fprintf(stdout, "  Cache %s: %s not found, skipping\n", cm.Name, resolveHostPath(cm.HostPath, home))
			}
		}
		for _, a := range active {
			var files int64
			var size int64
			if a.files != nil {
				// Shallow: only top-level files are copied.
				files = int64(len(a.files))
				for _, f := range a.files {
					if info, err := os.Stat(filepath.Join(a.hostPath, f)); err == nil {
						size += info.Size()
					}
				}
			} else {
				files, size = dirStats(a.hostPath)
			}
			_, _ = fmt.Fprintf(stdout, "  Cache %s: %s files, %s\n", a.cm.Name, formatCount(files), FormatBytes(size))
		}
	}

	// Generate a temporary build context containing SSH keys and a Dockerfile.
	// Cache directories are mounted via --build-context so their contents are
	// read directly from the host without copying into the context dir.
	tmpDir, err := os.MkdirTemp("", "md-specialized-*")
	if err != nil {
		return fmt.Errorf("creating build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub", "authorized_keys"} {
		data, err := os.ReadFile(filepath.Join(keysDir, name))
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, filepath.Base(name)), data, 0o600); err != nil { //nolint:gosec // name is from a hardcoded list
			return fmt.Errorf("staging %s: %w", name, err)
		}
	}

	df := generateDockerfile(baseImage, active, dirs, baseDigest, contextSHA, activeKey, manifestDigest)
	slog.DebugContext(ctx, "md", "msg", "generated Dockerfile", "content", df)

	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(df), 0o644); err != nil {
		return fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Build the image. --no-cache forces all layers to rebuild (prevents stale
	// results). We omit --pull so BuildKit won't re-pull the base (we already
	// pulled above).
	buildCmd := []string{rt, "build", "--no-cache", "--platform", "linux/" + arch, "-t", imageName}
	for _, a := range active {
		buildCmd = append(buildCmd, "--build-context", fmt.Sprintf("cache-%s=%s", a.cm.Name, a.hostPath))
	}
	buildCmd = append(buildCmd, tmpDir)

	if quiet {
		if _, err := runCmd(ctx, "", buildCmd); err != nil {
			buildErr := cmdErrWithStderr("building image", err)
			if isStaleBuilderCacheErr(buildErr) {
				if _, pruneErr := runCmd(ctx, "", []string{rt, "builder", "prune", "-f"}); pruneErr != nil {
					return buildErr
				}
				if _, err2 := runCmd(ctx, "", buildCmd); err2 != nil {
					return cmdErrWithStderr("building image", err2)
				}
				return nil
			}
			return buildErr
		}
	} else {
		if err := runCmdOut(ctx, "", buildCmd, stdout, stderr); err != nil {
			buildErr := fmt.Errorf("building image: %w", err)
			if isStaleBuilderCacheErr(buildErr) {
				_, _ = fmt.Fprintln(stdout, "- Stale BuildKit cache detected; pruning and retrying ...")
				if _, pruneErr := runCmd(ctx, "", []string{rt, "builder", "prune", "-f"}); pruneErr != nil {
					return buildErr
				}
				if err2 := runCmdOut(ctx, "", buildCmd, stdout, stderr); err2 != nil {
					return fmt.Errorf("building image: %w", err2)
				}
				return nil
			}
			return buildErr
		}
	}
	return nil
}

// isStaleBuilderCacheErr reports whether err looks like a BuildKit cache
// corruption error caused by a file that existed in a previous build context
// snapshot but has since been deleted from the host. This most commonly affects
// shallow caches: because each file gets its own COPY instruction, BuildKit
// stores per-file refs; if any of those files is later deleted, the next build
// fails to checksum the stale ref. Non-shallow caches copy "." so deleted files
// fall out naturally without leaving dangling refs.
func isStaleBuilderCacheErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "failed to compute cache key") || strings.Contains(s, "failed to calculate checksum of ref")
}

// dirStats returns the number of regular files and total byte size under dir.
// Unreadable entries are silently skipped.
func dirStats(dir string) (files, n int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				files++
				n += info.Size()
			}
		}
		return nil
	})
	return files, n
}

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

// resolveHostPath expands a leading "~/" (or "~\" on Windows) to home;
// absolute paths are returned unchanged.
func resolveHostPath(p, home string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.ToSlash(filepath.Join(home, p[2:]))
	}
	return p
}

// launchContainer starts the Docker container, queries mapped ports, writes
// SSH config, and sets up host-side git remotes. It does NOT wait for SSH.
// Port and creation-time results are stored directly on c (launchSSHPort,
// launchVNCPort, CreatedAt) so that connectContainer can complete startup.
func launchContainer(ctx context.Context, stdout, stderr io.Writer, c *Container, opts *StartOpts, imageName string) error {
	if len(c.Repos) > 1000 {
		return fmt.Errorf("too many repositories: %d (max 1000)", len(c.Repos))
	}
	rt := c.Runtime
	var dockerArgs []string
	dockerArgs = append(dockerArgs, rt, "run", "-d",
		"--name", c.Name, "--hostname", c.Name,
		"-p", "127.0.0.1::22")

	if opts.MaxCPUs > 0 {
		dockerArgs = append(dockerArgs, "--cpus", strconv.Itoa(opts.MaxCPUs))
	}

	if opts.Display {
		dockerArgs = append(dockerArgs, "-p", "127.0.0.1::5901", "-e", "MD_DISPLAY=1")
	}

	if kvmAvailable() {
		dockerArgs = append(dockerArgs, "--device=/dev/kvm")
	}
	// Localtime.
	if runtime.GOOS == "linux" {
		dockerArgs = append(dockerArgs, "-v", "/etc/localtime:/etc/localtime:ro")
	}
	// Sandbox capabilities.
	// - SYS_PTRACE: needed for strace/debuggers. Scoped to the container's
	//   PID namespace — cannot attach to host processes.
	// - seccomp=unconfined: disables the syscall allowlist so strace, bpf,
	//   and Chrome's sandbox work. Does NOT grant capabilities — the
	//   capability set still limits what the process can do.
	dockerArgs = append(dockerArgs,
		"--cap-add=SYS_PTRACE",
		"--security-opt", "seccomp=unconfined")
	// - apparmor=unconfined: disables AppArmor's mandatory-access-control
	//   profile so Chrome can create namespaces and sandboxed processes can
	//   access /proc. Docker-only; podman uses SELinux and passing this
	//   option can hang on kernel security filesystem access.
	if rt != "podman" {
		dockerArgs = append(dockerArgs, "--security-opt", "apparmor=unconfined")
	}

	// Rootless podman: --userns=keep-id maps host UID to same UID inside the
	// container so bind-mounted configs are writable. --user 0:0 keeps
	// start.sh running as root for privileged setup (groupmod, sshd, dbus).
	// Rootless Docker is handled inside start.sh via /proc/self/uid_map
	// detection since Docker lacks --userns=keep-id.
	if isRootlessPodman(rt) {
		dockerArgs = append(dockerArgs, "--userns=keep-id", "--user", "0:0")
	}

	// Tailscale.
	if opts.Tailscale {
		dockerArgs = append(dockerArgs,
			"--cap-add=NET_ADMIN", "--cap-add=NET_RAW", "--cap-add=MKNOD",
			"-e", "MD_TAILSCALE=1")
		if opts.TailscaleAuthKey != "" {
			dockerArgs = append(dockerArgs, "-e", "TAILSCALE_AUTHKEY="+opts.TailscaleAuthKey)
		}
		if c.tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "-e", "MD_TAILSCALE_EPHEMERAL=1")
		}
	}

	// USB passthrough (Linux only; Docker Desktop on macOS/Windows runs in a
	// VM that cannot access host USB devices). Use a bind mount + cgroup
	// rule so that devices plugged in after container start are visible.
	if opts.USB {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("--usb requires Linux; Docker Desktop on %s cannot pass through host USB devices", runtime.GOOS)
		}
		dockerArgs = append(dockerArgs,
			"-v", "/dev/bus/usb:/dev/bus/usb",
			"--device-cgroup-rule=c 189:* rwm")
	}

	// Agent config mounts: always-mounted paths plus caller-specified harness paths.
	combined := mergePaths(opts.AgentPaths)
	home := c.Home
	xdgConfig := c.XDGConfigHome
	xdgData := c.XDGDataHome
	xdgState := c.XDGStateHome
	for _, p := range combined.HomePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(home, p)+":/home/user/"+p)
	}
	for _, p := range combined.XDGConfigPaths {
		ro := ""
		if p == "md" {
			ro = ":ro"
		}
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgConfig, p)+":/home/user/.config/"+p+ro)
	}
	for _, p := range combined.LocalSharePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgData, p)+":/home/user/.local/share/"+p)
	}
	for _, p := range combined.LocalStatePaths {
		dockerArgs = append(dockerArgs, "-v", filepath.Join(xdgState, p)+":/home/user/.local/state/"+p)
	}

	// Set md metadata labels.
	if reposJSON, err := json.Marshal(c.Repos); err == nil {
		// Base64-encode so commas in JSON don't corrupt the comma-separated
		// label parsing in unmarshalContainer.
		dockerArgs = append(dockerArgs, "--label", "md.repos="+base64.StdEncoding.EncodeToString(reposJSON))
	}
	if opts.Display {
		dockerArgs = append(dockerArgs, "--label", "md.display=1")
	}
	if opts.Tailscale {
		dockerArgs = append(dockerArgs, "--label", "md.tailscale=1")
		if c.tailscaleEphemeral {
			dockerArgs = append(dockerArgs, "--label", "md.tailscale_ephemeral=1")
		}
	}
	if opts.USB {
		dockerArgs = append(dockerArgs, "--label", "md.usb=1")
	}
	for _, l := range opts.Labels {
		dockerArgs = append(dockerArgs, "--label", l)
	}
	dockerArgs = append(dockerArgs, opts.ExtraRunArgs...)
	dockerArgs = append(dockerArgs, imageName)

	if opts.Quiet {
		if _, err := runCmd(ctx, "", dockerArgs); err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "- Starting container %s ... ", c.Name)
		if err := runCmdOut(ctx, "", dockerArgs, stdout, stderr); err != nil {
			_, _ = fmt.Fprintln(stdout)
			return fmt.Errorf("starting container: %w", err)
		}
	}

	// Get SSH port and creation time.
	port, err := getHostPort(ctx, rt, c.Name, "22/tcp")
	if err != nil {
		return fmt.Errorf("getting SSH port: %w", err)
	}
	c.SSHPort = port
	if !opts.Quiet {
		_, _ = fmt.Fprintf(stdout, "- Found ssh port %d\n", port)
	}
	createdStr, err := runCmd(ctx, "", []string{rt, "inspect", "--format", "{{.Created}}", c.Name})
	if err != nil {
		return fmt.Errorf("getting container creation time: %w", err)
	}
	created, err := parseCreatedAt(createdStr)
	if err != nil {
		return fmt.Errorf("parsing container creation time %q: %w", createdStr, err)
	}
	c.CreatedAt = created

	// Get VNC port if display enabled.
	if opts.Display {
		vncPort, _ := getHostPort(ctx, rt, c.Name, "5901/tcp")
		c.VNCPort = vncPort
		if vncPort != 0 && !opts.Quiet {
			_, _ = fmt.Fprintf(stdout, "- Found VNC port %d (display :1)\n", vncPort)
		}
	}

	// Write SSH config.
	sshConfigDir := filepath.Join(home, ".ssh", "config.d")
	if err := os.MkdirAll(sshConfigDir, 0o700); err != nil {
		return err
	}
	knownHostsPath := filepath.Join(sshConfigDir, c.Name+".known_hosts")
	hostPubKey, err := os.ReadFile(c.HostKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("reading host public key: %w", err)
	}
	if err := writeSSHConfig(sshConfigDir, c.Name, port, c.UserKeyPath, knownHostsPath, c.ControlMaster); err != nil {
		return err
	}
	if err := writeKnownHosts(knownHostsPath, port, strings.TrimSpace(string(hostPubKey))); err != nil {
		return err
	}

	// Set up git remotes for all repos before waiting for SSH, so they are
	// ready to push as soon as the connection is established.
	if len(c.Repos) > 0 {
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- git clone into container ...")
		}
		for _, r := range c.Repos {
			rName := r.Name()
			_, _ = runCmd(ctx, r.GitRoot, []string{"git", "remote", "rm", c.Name})
			if err := runCmdOut(ctx, r.GitRoot, []string{"git", "remote", "add", c.Name, "user@" + c.Name + ":/home/user/src/" + rName}, stdout, stderr); err != nil {
				return fmt.Errorf("adding git remote for %s: %w", rName, err)
			}
		}
	}
	return nil
}

// waitForTCP polls until a TCP connection to addr succeeds or the deadline is
// exceeded.
func waitForTCP(ctx context.Context, addr string, deadline time.Time) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for TCP %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// connectContainer waits for SSH, pushes repos into the container, and
// handles .env and Tailscale auth. Must be called after launchContainer.
//
// The task branch and default branch are pushed in parallel to reduce latency.
func connectContainer(ctx context.Context, stdout, stderr io.Writer, c *Container, opts *StartOpts) (*StartResult, error) {
	result := &StartResult{}

	// Phase 1: wait for TCP port to accept connections.
	addr := fmt.Sprintf("localhost:%d", c.SSHPort)
	deadline := time.Now().Add(30 * time.Second)
	if err := waitForTCP(ctx, addr, deadline); err != nil {
		return nil, err
	}

	// Send .env into the container via ssh+stdin — this is the first SSH
	// operation and doubles as the handshake readiness check. Using ssh
	// instead of scp gives reliable exit code 255 on connection errors.
	// If no .env exists locally the container still gets an empty file.
	var envContent []byte
	for _, r := range c.Repos {
		data, err := os.ReadFile(filepath.Join(r.GitRoot, ".env"))
		if err != nil {
			continue
		}
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		envContent = append(envContent, data...)
	}
	if len(envContent) > 0 && !opts.Quiet {
		_, _ = fmt.Fprintln(stdout, "- sending .env into container ...")
	}
	if len(opts.ExtraEnv) > 0 {
		if len(envContent) > 0 && envContent[len(envContent)-1] != '\n' {
			envContent = append(envContent, '\n')
		}
		for _, kv := range opts.ExtraEnv {
			envContent = append(envContent, []byte(kv+"\n")...)
		}
		if !opts.Quiet {
			_, _ = fmt.Fprintln(stdout, "- injecting extra env vars into container ...")
		}
	}
	sshEnvArgs := c.SSHCommand(c.Name, "cat > /home/user/.env")
	for {
		cmd := exec.CommandContext(ctx, sshEnvArgs[0], sshEnvArgs[1:]...)
		cmd.Stdin = bytes.NewReader(envContent)
		out, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 || time.Now().After(deadline) {
			return nil, fmt.Errorf("copying .env: %w\n%s", err, out)
		}
	}

	// Push all repos into the container in parallel. Each repo pushes to a
	// distinct path (~/src/<name>) so there are no cross-repo conflicts.
	if len(c.Repos) > 0 {
		eg, egCtx := errgroup.WithContext(ctx)
		for repoIdx := range c.Repos {
			eg.Go(func() error {
				rName := c.Repos[repoIdx].Name()
				rRepo := shellQuote(rName)
				rBranch := shellQuote(c.Repos[repoIdx].Branch)

				if err := runCmdOut(egCtx, "", c.SSHCommand(c.Name, "git init -q ~/src/"+rRepo), stdout, stderr); err != nil {
					return fmt.Errorf("init repo %s in container: %w", rName, err)
				}

				// Resolve defaults concurrently with the base push (no git I/O to the
				// container), but serialize the two pushes: concurrent receive-pack
				// on the same repo can race on pack migration (.keep file conflicts).
				resolveErr := make(chan error, 1)
				go func() {
					resolveErr <- c.Repos[repoIdx].resolveDefaults(egCtx)
				}()

				if err := runCmdOut(egCtx, c.Repos[repoIdx].GitRoot, []string{
					"git", "push", "-q", c.Name,
					c.Repos[repoIdx].Branch + ":refs/heads/base",
				}, stdout, stderr); err != nil {
					return fmt.Errorf("push repo %s: %w", rName, err)
				}
				if err := runCmdOut(egCtx, "", c.SSHCommand(c.Name,
					"cd ~/src/"+rRepo+
						" && git branch -q --track "+rBranch+" base"+
						" && git switch -q "+rBranch), stdout, stderr); err != nil {
					return err
				}

				if err := <-resolveErr; err != nil {
					return fmt.Errorf("resolve defaults for %s: %w", rName, err)
				}
				if err := c.SyncDefaultBranch(egCtx, repoIdx); err != nil {
					return err
				}

				if err := c.pushSubmodules(egCtx, stdout, stderr, "/home/user/src/"+rName, c.Repos[repoIdx].GitRoot, opts.Quiet); err != nil {
					return fmt.Errorf("push submodules for %s: %w", rName, err)
				}

				// resolveDefaults ran above, so DefaultRemote is set.
				originURL, err := runCmd(egCtx, c.Repos[repoIdx].GitRoot, []string{"git", "remote", "get-url", c.Repos[repoIdx].DefaultRemote})
				if err == nil && originURL != "" {
					httpsURL := convertGitURLToHTTPS(originURL)
					_, _ = runCmd(egCtx, "", c.SSHCommand(c.Name, "cd ~/src/"+rRepo+" && git remote add origin "+shellQuote(httpsURL)))
					if !opts.Quiet {
						_, _ = fmt.Fprintf(stdout, "- Set %s origin to %s\n", rName, httpsURL)
					}
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	// Wait for Tailscale auth URL if needed.
	if opts.Tailscale && opts.TailscaleAuthKey == "" {
		tailArgs := c.SSHCommand(c.Name, "tail -f /tmp/tailscale_auth_url")
		cmd := exec.CommandContext(ctx, tailArgs[0], tailArgs[1:]...)
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
