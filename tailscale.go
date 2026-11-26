// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tailscale authentication and networking.

package md

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTailscaleTag is the tag of a container's node when the caller names
// none.
const DefaultTailscaleTag = "tag:md"

// ValidateTailscaleTags rejects a tag that Tailscale rejects: "tag:" followed
// by a letter, then letters, digits, or dashes. It mirrors tailcfg.CheckTag.
func ValidateTailscaleTags(tags []string) error {
	for _, tag := range tags {
		name, ok := strings.CutPrefix(tag, "tag:")
		valid := ok && name != "" && isASCIILetter(name[0])
		for i := 0; valid && i < len(name); i++ {
			b := name[i]
			valid = isASCIILetter(b) || '0' <= b && b <= '9' || b == '-'
		}
		if !valid {
			return fmt.Errorf("invalid Tailscale tag %q: want tag: followed by a letter, then letters, digits, or dashes", tag)
		}
	}
	return nil
}

func isASCIILetter(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// tailscaleTagsOrDefault returns tags, or DefaultTailscaleTag when there are
// none.
func tailscaleTagsOrDefault(tags []string) []string {
	if len(tags) == 0 {
		return []string{DefaultTailscaleTag}
	}
	return tags
}

// tailscaleAuthKeyExpiry is the lifetime of the auth keys md mints. A launch
// uses its key right after minting it.
const tailscaleAuthKeyExpiry = 5 * time.Minute

// tailscaleKey is the Tailscale API v2 "Key" object: an API access token,
// auth key, OAuth client, or federated identity. md sends it to create an auth
// key and decodes the created key from the response. Fields that apply to one
// kind are empty for the others.
//
// https://tailscale.com/api#tag/keys
type tailscaleKey struct {
	ID string `json:"id,omitempty"`
	// Key is the secret; the API returns it only when it creates the key.
	Key string `json:"key,omitempty"`
	// KeyType is "api", "auth", "client", or "federated".
	KeyType string `json:"keyType,omitempty"`
	// ExpirySeconds is the key's lifetime.
	ExpirySeconds int64     `json:"expirySeconds,omitempty"`
	Created       time.Time `json:"created,omitzero"`
	Updated       time.Time `json:"updated,omitzero"`
	// Expires is zero for a key that never expires.
	Expires time.Time `json:"expires,omitzero"`
	// Revoked is when the key was revoked; zero when it was not.
	Revoked      time.Time                 `json:"revoked,omitzero"`
	Capabilities *tailscaleKeyCapabilities `json:"capabilities,omitempty"`
	// Scopes are the permissions of an API access token, OAuth client, or
	// federated identity, such as "all" or "all:read".
	Scopes []string `json:"scopes,omitempty"`
	// Tags are the tags an OAuth client or federated identity must apply.
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	// Invalid is true for a revoked or expired key.
	Invalid bool `json:"invalid,omitempty"`
	// UserID is the creator; empty for keys that trust credentials created.
	UserID string `json:"userId,omitempty"`
	// Audience, Issuer, Subject, and CustomClaimRules match OIDC identity
	// tokens of a federated identity.
	Audience         string            `json:"audience,omitempty"`
	Issuer           string            `json:"issuer,omitempty"`
	Subject          string            `json:"subject,omitempty"`
	CustomClaimRules map[string]string `json:"customClaimRules,omitempty"`
}

// tailscaleKeyCapabilities maps resources to an auth key's permitted actions.
type tailscaleKeyCapabilities struct {
	Devices tailscaleKeyDevices `json:"devices"`
}

// tailscaleKeyDevices holds an auth key's permissions over devices.
type tailscaleKeyDevices struct {
	Create tailscaleKeyDeviceCreate `json:"create"`
}

// tailscaleKeyDeviceCreate describes the devices an auth key registers.
type tailscaleKeyDeviceCreate struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags,omitempty"`
	// NodeAttributes are device posture attributes set on registered devices.
	NodeAttributes map[string]any `json:"nodeAttributes,omitempty"`
}

// generateTailscaleAuthKey creates a one-time ephemeral pre-authorized
// Tailscale auth key via the API. The node it registers carries tags.
func generateTailscaleAuthKey(ctx context.Context, apiKey string, tags []string) (string, error) {
	if err := ValidateTailscaleTags(tags); err != nil {
		return "", err
	}
	body, err := json.Marshal(&tailscaleKey{
		Capabilities: &tailscaleKeyCapabilities{Devices: tailscaleKeyDevices{Create: tailscaleKeyDeviceCreate{
			Ephemeral:     true,
			Preauthorized: true,
			Tags:          tags,
		}}},
		ExpirySeconds: int64(tailscaleAuthKeyExpiry / time.Second),
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
			return "", fmt.Errorf("%s not allowed, the token's owner must own them in the tailnet policy, see https://console.tailscale.com/admin/acls/visual/tags", strings.Join(tags, ", "))
		}
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, s)
	}
	// Lenient: the API may add fields. tailscaleKey decoded a live response
	// with DisallowUnknownFields.
	var result tailscaleKey
	if err := json.Unmarshal(respBody, &result); err != nil || result.Key == "" {
		return "", errors.New("no key in response")
	}
	return result.Key, nil
}
