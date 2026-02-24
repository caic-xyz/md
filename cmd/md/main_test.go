// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package main

import (
	"testing"

	"github.com/caic-xyz/md"
)

func TestResolveCaches(t *testing.T) {
	allNames := func(caches []md.CacheMount) []string {
		names := make([]string, len(caches))
		for i, c := range caches {
			names[i] = c.Name
		}
		return names
	}

	t.Run("default_includes_all_well_known", func(t *testing.T) {
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
		_, err := resolveCaches(nil, []string{"nonexistent"}, false)
		if err == nil {
			t.Fatal("expected error for unknown --no-cache name")
		}
	})

	t.Run("custom_cache_added", func(t *testing.T) {
		got, err := resolveCaches([]string{"/host/path:/container/path"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostPath != "/host/path" || got[0].ContainerPath != "/container/path" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("no_caches_plus_cache_readds_well_known", func(t *testing.T) {
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
		got, err := resolveCaches([]string{"/host:/cnt:ro"}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[0].ReadOnly {
			t.Errorf("expected read-only cache, got %+v", got)
		}
	})

	t.Run("invalid_custom_spec_errors", func(t *testing.T) {
		_, err := resolveCaches([]string{"notapath"}, nil, true)
		if err == nil {
			t.Fatal("expected error for invalid custom spec")
		}
	})
}
