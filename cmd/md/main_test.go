// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for the md CLI tool.

package main

import (
	"testing"

	"github.com/caic-xyz/md"
)

func TestResolveCaches(t *testing.T) {
	t.Parallel()
	allNames := func(caches []md.CacheMount) []string {
		names := make([]string, len(caches))
		for i, c := range caches {
			names[i] = c.Name
		}
		return names
	}

	t.Run("default_includes_all_well_known", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		// Must be non-nil (not nil) so imageBuildNeeded always checks the key.
		if got == nil {
			t.Fatal("expected non-nil slice")
		}
		// Every well-known cache must appear.
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for name, mounts := range md.WellKnownCaches {
			for _, m := range mounts {
				if !present[m.Name] {
					t.Errorf("well-known cache %q (%s) missing from default result", name, m.Name)
				}
			}
		}
	})

	t.Run("no_caches_returns_empty_non_nil", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("expected non-nil slice")
		}
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", allNames(got))
		}
	})

	t.Run("no_cache_excludes_named", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches(nil, []string{"go-mod"}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.Name == "go-mod" {
				t.Error("go-mod should have been excluded")
			}
		}
		// Other well-known caches should still be present.
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for name, mounts := range md.WellKnownCaches {
			if name == "go-mod" {
				continue
			}
			for _, m := range mounts {
				if !present[m.Name] {
					t.Errorf("cache %q unexpectedly absent", m.Name)
				}
			}
		}
	})

	t.Run("no_cache_unknown_name_errors", func(t *testing.T) {
		t.Parallel()
		_, err := resolveCaches(nil, []string{"nonexistent"}, false)
		if err == nil {
			t.Fatal("expected error for unknown --no-cache name")
		}
	})

	t.Run("custom_cache_added", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"/host/path:/container/path"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "/host/path" || got[0].ContainerPath != "/container/path" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("custom_cache_expands_container_tilde", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"~/.cache/tool:~/.cache/tool"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "~/.cache/tool" || got[0].ContainerPath != "/home/user/.cache/tool" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("custom_cache_expands_bare_container_tilde", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"~:~"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "home" || got[0].HostPath != "~" || got[0].ContainerPath != "/home/user" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("no_caches_plus_cache_readds_well_known", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"go-mod"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		present := make(map[string]bool)
		for _, c := range got {
			present[c.Name] = true
		}
		for _, m := range md.WellKnownCaches["go-mod"] {
			if !present[m.Name] {
				t.Errorf("go-mod cache %q should have been re-added", m.Name)
			}
		}
		// No other well-known caches.
		for name, mounts := range md.WellKnownCaches {
			if name == "go-mod" {
				continue
			}
			for _, m := range mounts {
				if present[m.Name] {
					t.Errorf("cache %q should not be present", m.Name)
				}
			}
		}
	})

	t.Run("no_duplicate_when_cache_already_default", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"go-mod"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, c := range got {
			if c.Name == "go-mod" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected go-mod exactly once, got %d", count)
		}
	})

	t.Run("custom_cache_ro", func(t *testing.T) {
		t.Parallel()
		got, err := resolveCaches([]string{"/host:/cnt:ro"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[0].ReadOnly {
			t.Errorf("expected read-only cache, got %+v", got)
		}
	})

	t.Run("invalid_custom_spec_errors", func(t *testing.T) {
		t.Parallel()
		_, err := resolveCaches([]string{"notapath"}, nil, true)
		if err == nil {
			t.Fatal("expected error for invalid custom spec")
		}
	})
}

func TestShellSplit(t *testing.T) {
	t.Parallel()
	t.Run("simple", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--memory 4g")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"--memory", "4g"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("single_arg", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--privileged")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "--privileged" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("equals_form", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("--memory=4g")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "--memory=4g" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("single_quotes", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("-v '/path with spaces:/container'")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "-v" || got[1] != "/path with spaces:/container" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("double_quotes", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit(`-e "FOO=hello world"`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "-e" || got[1] != "FOO=hello world" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("backslash_escape", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit(`--label key=val\ ue`)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "--label" || got[1] != "key=val ue" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got, err := shellSplit("")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("unterminated_single_quote", func(t *testing.T) {
		t.Parallel()
		_, err := shellSplit("--flag 'oops")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("unterminated_double_quote", func(t *testing.T) {
		t.Parallel()
		_, err := shellSplit(`--flag "oops`)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
