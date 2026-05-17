package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigTargetTimingInheritsGlobalDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"probe_interval": "45s",
		"probe_timeout": "7s",
		"targets": [
			{
				"name": "a",
				"url": "https://example.com/a",
				"expected_body_contains": "ok"
			},
			{
				"name": "b",
				"url": "https://example.com/b",
				"expected_body_contains": "ok"
			}
		]
	}`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	for _, target := range cfg.Targets {
		if target.ProbeInterval != 45*time.Second {
			t.Fatalf("%s ProbeInterval = %v, want 45s", target.Name, target.ProbeInterval)
		}
		if target.ProbeTimeout != 7*time.Second {
			t.Fatalf("%s ProbeTimeout = %v, want 7s", target.Name, target.ProbeTimeout)
		}
	}
}

func TestLoadConfigTargetTimingOverridesGlobalDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"probe_interval": "45s",
		"probe_timeout": "7s",
		"targets": [
			{
				"name": "override",
				"url": "https://example.com/override",
				"probe_interval": "5s",
				"probe_timeout": "2s",
				"expected_body_contains": "ok"
			},
			{
				"name": "inherit",
				"url": "https://example.com/inherit",
				"expected_body_contains": "ok"
			}
		]
	}`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	override := cfg.Targets[0]
	if override.ProbeInterval != 5*time.Second {
		t.Fatalf("override ProbeInterval = %v, want 5s", override.ProbeInterval)
	}
	if override.ProbeTimeout != 2*time.Second {
		t.Fatalf("override ProbeTimeout = %v, want 2s", override.ProbeTimeout)
	}

	inherit := cfg.Targets[1]
	if inherit.ProbeInterval != 45*time.Second {
		t.Fatalf("inherit ProbeInterval = %v, want 45s", inherit.ProbeInterval)
	}
	if inherit.ProbeTimeout != 7*time.Second {
		t.Fatalf("inherit ProbeTimeout = %v, want 7s", inherit.ProbeTimeout)
	}
}

func TestLoadConfigTargetTimingUsesBuiltInDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"targets": [
			{
				"name": "default",
				"url": "https://example.com/default",
				"expected_body_contains": "ok"
			}
		]
	}`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	target := cfg.Targets[0]
	if target.ProbeInterval != defaultProbeInterval {
		t.Fatalf("ProbeInterval = %v, want %v", target.ProbeInterval, defaultProbeInterval)
	}
	if target.ProbeTimeout != defaultProbeTimeout {
		t.Fatalf("ProbeTimeout = %v, want %v", target.ProbeTimeout, defaultProbeTimeout)
	}
}

func TestLoadConfigTargetTimingValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "invalid interval",
			body: `{
				"targets": [
					{
						"url": "https://example.com",
						"probe_interval": "bad",
						"expected_body_contains": "ok"
					}
				]
			}`,
			want: "targets[0].probe_interval:",
		},
		{
			name: "non positive interval",
			body: `{
				"targets": [
					{
						"url": "https://example.com",
						"probe_interval": "0s",
						"expected_body_contains": "ok"
					}
				]
			}`,
			want: "targets[0].probe_interval must be greater than 0",
		},
		{
			name: "invalid timeout",
			body: `{
				"targets": [
					{
						"url": "https://example.com",
						"probe_timeout": "bad",
						"expected_body_contains": "ok"
					}
				]
			}`,
			want: "targets[0].probe_timeout:",
		},
		{
			name: "non positive timeout",
			body: `{
				"targets": [
					{
						"url": "https://example.com",
						"probe_timeout": "-1s",
						"expected_body_contains": "ok"
					}
				]
			}`,
			want: "targets[0].probe_timeout must be greater than 0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.body)
			_, err := loadConfig(path)
			if err == nil {
				t.Fatalf("loadConfig() error = nil, want %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadConfig() error = %q, want substring %q", err.Error(), test.want)
			}
		})
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
