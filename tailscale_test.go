// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for tailscale.go

package md

import "testing"

func TestTailscaleDeviceIDFromStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  string
		want    string
		wantErr bool
	}{
		{
			name:   "valid",
			status: `{"Self":{"ID":"n123456CNTRL","DNSName":"md-test.tailnet.ts.net."}}`,
			want:   "n123456CNTRL",
		},
		{
			name:   "missing self",
			status: `{}`,
		},
		{
			name:    "bad json",
			status:  `{`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tailscaleDeviceIDFromStatus(tc.status)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("device ID = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestDeleteTailscaleDevice(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if err := deleteTailscaleDevice(t.Context(), "", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if err := deleteTailscaleDevice(t.Context(), "", "n123456CNTRL"); err == nil {
			t.Fatal("expected missing API key error")
		}
	})
}
