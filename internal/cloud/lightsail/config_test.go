package lightsail

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvDisabledDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("disabled defaults: %v", err)
	}
	if cfg.Enabled || len(cfg.AllowedRegions) != 0 || cfg.SSHSourceCIDR != "" {
		t.Fatalf("disabled config has cloud scope: %+v", cfg)
	}
	if cfg.WorkerInterval != 5*time.Second || cfg.AWSOperationTimeout != 15*time.Minute || cfg.SSHConnectTimeout != 30*time.Second || cfg.ScriptTimeout != 20*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestConfigFromEnvEnabledCanonicalizesRegionsAndLiteralHostCIDR(t *testing.T) {
	values := map[string]string{
		"LIGHTSAIL_ENABLED":         "true",
		"LIGHTSAIL_ALLOWED_REGIONS": "ap-southeast-1, eu-west-1,ap-southeast-1",
		"LIGHTSAIL_SSH_SOURCE_CIDR": "2001:db8::5/128",
	}
	cfg, err := ConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("valid enabled config: %v", err)
	}
	if !cfg.Enabled || !reflect.DeepEqual(cfg.AllowedRegions, []string{"ap-southeast-1", "eu-west-1"}) || cfg.SSHSourceCIDR != "2001:db8::5/128" {
		t.Fatalf("unexpected canonical config: %+v", cfg)
	}
}

func TestConfigFromEnvAcceptsBoundedDurationOverrides(t *testing.T) {
	values := map[string]string{
		"LIGHTSAIL_ENABLED":               "true",
		"LIGHTSAIL_ALLOWED_REGIONS":       "us-east-1",
		"LIGHTSAIL_SSH_SOURCE_CIDR":       "192.0.2.7/32",
		"LIGHTSAIL_WORKER_INTERVAL":       "1s",
		"LIGHTSAIL_AWS_OPERATION_TIMEOUT": "1h",
		"LIGHTSAIL_SSH_CONNECT_TIMEOUT":   "2m",
		"LIGHTSAIL_SCRIPT_TIMEOUT":        "2h",
	}
	cfg, err := ConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("bounded overrides: %v", err)
	}
	if cfg.WorkerInterval != time.Second || cfg.AWSOperationTimeout != time.Hour || cfg.SSHConnectTimeout != 2*time.Minute || cfg.ScriptTimeout != 2*time.Hour {
		t.Fatalf("overrides not loaded: %+v", cfg)
	}
}

func TestConfigFromEnvRejectsAmbiguousOrPartialConfiguration(t *testing.T) {
	valid := map[string]string{
		"LIGHTSAIL_ENABLED":         "true",
		"LIGHTSAIL_ALLOWED_REGIONS": "us-east-1",
		"LIGHTSAIL_SSH_SOURCE_CIDR": "192.0.2.7/32",
	}
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "boolean", key: "LIGHTSAIL_ENABLED", value: "1"},
		{name: "missing regions", key: "LIGHTSAIL_ALLOWED_REGIONS", value: ""},
		{name: "uppercase region", key: "LIGHTSAIL_ALLOWED_REGIONS", value: "US-EAST-1"},
		{name: "malformed region", key: "LIGHTSAIL_ALLOWED_REGIONS", value: "us-east"},
		{name: "network cidr", key: "LIGHTSAIL_SSH_SOURCE_CIDR", value: "192.0.2.0/24"},
		{name: "noncanonical host", key: "LIGHTSAIL_SSH_SOURCE_CIDR", value: "192.0.2.007/32"},
		{name: "worker too short", key: "LIGHTSAIL_WORKER_INTERVAL", value: "999ms"},
		{name: "operation too long", key: "LIGHTSAIL_AWS_OPERATION_TIMEOUT", value: "61m"},
		{name: "connect too long", key: "LIGHTSAIL_SSH_CONNECT_TIMEOUT", value: "5m1s"},
		{name: "script too long", key: "LIGHTSAIL_SCRIPT_TIMEOUT", value: "2h1s"},
		{name: "unknown duration", key: "LIGHTSAIL_SCRIPT_TIMEOUT", value: "forever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := make(map[string]string, len(valid))
			for key, value := range valid {
				values[key] = value
			}
			values[tc.key] = tc.value
			_, err := ConfigFromEnv(func(key string) string { return values[key] })
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error disclosed environment value: %v", err)
			}
		})
	}
}

func TestConfigFromEnvRejectsDisabledPartialConfiguration(t *testing.T) {
	for _, key := range []string{
		"LIGHTSAIL_ALLOWED_REGIONS",
		"LIGHTSAIL_SSH_SOURCE_CIDR",
		"LIGHTSAIL_WORKER_INTERVAL",
		"LIGHTSAIL_AWS_OPERATION_TIMEOUT",
		"LIGHTSAIL_SSH_CONNECT_TIMEOUT",
		"LIGHTSAIL_SCRIPT_TIMEOUT",
	} {
		t.Run(key, func(t *testing.T) {
			_, err := ConfigFromEnv(func(candidate string) string {
				if candidate == "LIGHTSAIL_ENABLED" {
					return "false"
				}
				if candidate == key {
					return "configured"
				}
				return ""
			})
			if err == nil {
				t.Fatal("accepted a disabled partial configuration")
			}
		})
	}
}
