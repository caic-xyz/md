// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for Tailscale tag validation and auth key minting.

package md

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestValidateTailscaleTags(t *testing.T) {
	t.Parallel()
	if err := ValidateTailscaleTags([]string{"tag:md", "tag:Ci-2", "tag:a"}); err != nil {
		t.Errorf("valid tags: %v", err)
	}
	for _, tag := range []string{"md", "tag:", "tag:1ci", "tag:-ci", "tag:ci_2", "tag:ci 2", "tag:ci,2", "tag:é"} {
		if err := ValidateTailscaleTags([]string{"tag:md", tag}); err == nil || !strings.Contains(err.Error(), tag) {
			t.Errorf("ValidateTailscaleTags(%q) = %v; want an error naming it", tag, err)
		}
	}
}

// rejectTagsTransport answers the key request as Tailscale does when the
// token's owner does not own a requested tag.
type rejectTagsTransport struct{}

func (rejectTagsTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body := `{"message":"requested tags [tag:ci] are invalid or not permitted"}`
	return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestGenerateTailscaleAuthKey(t *testing.T) { //nolint:paralleltest // replaces the process-wide default HTTP transport.
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	t.Run("invalid_tag", func(t *testing.T) { //nolint:paralleltest // shares the process-wide default HTTP transport.
		http.DefaultTransport = rejectTagsTransport{}
		if _, err := generateTailscaleAuthKey(t.Context(), "tskey-api-test", []string{"ci"}); err == nil || !strings.Contains(err.Error(), "invalid Tailscale tag") {
			t.Fatalf("err = %v; want the tag rejected before the request", err)
		}
	})
	t.Run("tag_not_permitted", func(t *testing.T) { //nolint:paralleltest // shares the process-wide default HTTP transport.
		http.DefaultTransport = rejectTagsTransport{}
		_, err := generateTailscaleAuthKey(t.Context(), "tskey-api-test", []string{"tag:ci", "tag:md"})
		if err == nil || !strings.Contains(err.Error(), "tag:ci, tag:md not allowed") {
			t.Fatalf("err = %v; want the requested tags named", err)
		}
	})
}
