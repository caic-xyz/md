// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tailscaleAuthKeyResponse struct {
	Key string `json:"key"`
}

// generateTailscaleAuthKey creates a one-time ephemeral pre-authorized
// Tailscale auth key via the API.
func generateTailscaleAuthKey(ctx context.Context, apiKey string) (string, error) {
	if apiKey == "" {
		return "", errors.New("no Tailscale API key provided, create an API access key at https://login.tailscale.com/admin/settings/keys")
	}
	body, err := json.Marshal(map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     true,
					"preauthorized": true,
					"tags":          []string{"tag:md"},
				},
			},
		},
		"expirySeconds": 300,
	})
	if err != nil {
		return "", err
	}
	const keysURL = "https://api.tailscale.com/api/v2/tailnet/-/keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keysURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		s := string(respBody)
		if strings.Contains(s, "tags") && strings.Contains(s, "invalid") {
			return "", errors.New("tag:md not allowed, add it at https://login.tailscale.com/admin/acls/visual/tags")
		}
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, s)
	}
	var result tailscaleAuthKeyResponse
	if err := json.Unmarshal(respBody, &result); err != nil || result.Key == "" {
		return "", errors.New("no key in response")
	}
	return result.Key, nil
}

// deleteTailscaleDevice deletes a Tailscale device using the API.
func deleteTailscaleDevice(ctx context.Context, apiKey, deviceID string) error {
	if apiKey == "" {
		return nil
	}
	if deviceID == "" {
		return nil
	}
	u, err := url.JoinPath("https://api.tailscale.com/api/v2/device", deviceID)
	if err != nil {
		return fmt.Errorf("building device URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting device: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleting device: API returned %d", resp.StatusCode)
	}
	return nil
}

// tailscaleUpStatus is the subset of `tailscale up --json` output we care about.
type tailscaleUpStatus struct {
	AuthURL      string `json:"AuthURL"`
	QR           string `json:"QR"`
	BackendState string `json:"BackendState"`
}
