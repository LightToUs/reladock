package lightsail

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

const (
	defaultWorkerInterval      = 5 * time.Second
	defaultAWSOperationTimeout = 15 * time.Minute
	defaultSSHConnectTimeout   = 30 * time.Second
	defaultScriptTimeout       = 20 * time.Minute
)

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)

type Config struct {
	Enabled             bool
	AllowedRegions      []string
	SSHSourceCIDR       string
	WorkerInterval      time.Duration
	AWSOperationTimeout time.Duration
	SSHConnectTimeout   time.Duration
	ScriptTimeout       time.Duration
}

func ConfigFromEnv(get func(string) string) (Config, error) {
	cfg := Config{
		WorkerInterval:      defaultWorkerInterval,
		AWSOperationTimeout: defaultAWSOperationTimeout,
		SSHConnectTimeout:   defaultSSHConnectTimeout,
		ScriptTimeout:       defaultScriptTimeout,
	}
	enabledValue := get("LIGHTSAIL_ENABLED")
	switch enabledValue {
	case "", "false":
		for _, key := range configKeysExceptEnabled() {
			if get(key) != "" {
				return Config{}, errors.New("Lightsail settings require LIGHTSAIL_ENABLED=true")
			}
		}
		return cfg, nil
	case "true":
		cfg.Enabled = true
	default:
		return Config{}, errors.New("LIGHTSAIL_ENABLED must be true or false")
	}

	regions, err := parseRegions(get("LIGHTSAIL_ALLOWED_REGIONS"))
	if err != nil {
		return Config{}, err
	}
	cfg.AllowedRegions = regions
	cfg.SSHSourceCIDR, err = parseHostCIDR(get("LIGHTSAIL_SSH_SOURCE_CIDR"))
	if err != nil {
		return Config{}, err
	}
	if cfg.WorkerInterval, err = durationSetting(get, "LIGHTSAIL_WORKER_INTERVAL", cfg.WorkerInterval, time.Second, 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.AWSOperationTimeout, err = durationSetting(get, "LIGHTSAIL_AWS_OPERATION_TIMEOUT", cfg.AWSOperationTimeout, time.Minute, time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.SSHConnectTimeout, err = durationSetting(get, "LIGHTSAIL_SSH_CONNECT_TIMEOUT", cfg.SSHConnectTimeout, time.Second, 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ScriptTimeout, err = durationSetting(get, "LIGHTSAIL_SCRIPT_TIMEOUT", cfg.ScriptTimeout, time.Minute, 2*time.Hour); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) AllowsRegion(region string) bool {
	for _, allowed := range c.AllowedRegions {
		if region == allowed {
			return true
		}
	}
	return false
}

func configKeysExceptEnabled() []string {
	return []string{
		"LIGHTSAIL_ALLOWED_REGIONS",
		"LIGHTSAIL_SSH_SOURCE_CIDR",
		"LIGHTSAIL_WORKER_INTERVAL",
		"LIGHTSAIL_AWS_OPERATION_TIMEOUT",
		"LIGHTSAIL_SSH_CONNECT_TIMEOUT",
		"LIGHTSAIL_SCRIPT_TIMEOUT",
	}
}

func parseRegions(value string) ([]string, error) {
	if value == "" {
		return nil, errors.New("LIGHTSAIL_ALLOWED_REGIONS is required when Lightsail is enabled")
	}
	seen := make(map[string]struct{})
	regions := make([]string, 0)
	for _, field := range strings.Split(value, ",") {
		region := strings.TrimSpace(field)
		if !regionPattern.MatchString(region) {
			return nil, errors.New("LIGHTSAIL_ALLOWED_REGIONS contains an invalid region code")
		}
		if _, exists := seen[region]; exists {
			continue
		}
		seen[region] = struct{}{}
		regions = append(regions, region)
	}
	if len(regions) == 0 {
		return nil, errors.New("LIGHTSAIL_ALLOWED_REGIONS requires at least one region")
	}
	return regions, nil
}

func parseHostCIDR(value string) (string, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Bits() != prefix.Addr().BitLen() || prefix.Masked().Addr() != prefix.Addr() || prefix.String() != value {
		return "", errors.New("LIGHTSAIL_SSH_SOURCE_CIDR must be one canonical literal IPv4 /32 or IPv6 /128")
	}
	return value, nil
}

func durationSetting(get func(string) string, key string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	value := get(key)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, errors.New(key + " is outside its allowed duration range")
	}
	return duration, nil
}
