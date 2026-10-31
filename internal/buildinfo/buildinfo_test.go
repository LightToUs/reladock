package buildinfo

import "testing"

func TestCurrentDefaultsToDevAndRejectsInvalidInjectedValues(t *testing.T) {
	original := ReleaseVersion
	t.Cleanup(func() { ReleaseVersion = original })

	ReleaseVersion = "dev"
	if got := Current(); got != "dev" {
		t.Fatalf("Current() = %q, want dev", got)
	}
	ReleaseVersion = "apply-opt-in/0.9.0"
	if got := Current(); got != "dev" {
		t.Fatalf("unsafe injected version escaped as %q", got)
	}
}

func TestParseProductionAcceptsOnlyStrictReleaseVersions(t *testing.T) {
	tests := []struct {
		name                string
		input               string
		major, minor, patch int
		ok                  bool
	}{
		{name: "release", input: "v0.9.0", major: 0, minor: 9, patch: 0, ok: true},
		{name: "next major", input: "v12.34.56", major: 12, minor: 34, patch: 56, ok: true},
		{name: "prerelease", input: "v0.9.0-rc.1"},
		{name: "mode prefix", input: "validation-only/0.9.0"},
		{name: "missing v", input: "0.9.0"},
		{name: "leading zero", input: "v0.09.0"},
		{name: "trailing data", input: "v0.9.0\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			major, minor, patch, ok := ParseProduction(test.input)
			if major != test.major || minor != test.minor || patch != test.patch || ok != test.ok {
				t.Fatalf("ParseProduction(%q) = %d,%d,%d,%v", test.input, major, minor, patch, ok)
			}
		})
	}
}

func TestCanUpgradeRequiresIncreasingVersionWithinSameMajor(t *testing.T) {
	tests := []struct {
		current, target string
		want            bool
	}{
		{current: "v0.8.0", target: "v0.9.0", want: true},
		{current: "v0.9.0", target: "v0.9.1", want: true},
		{current: "v0.9.0", target: "v0.10.0", want: true},
		{current: "v0.9.0", target: "v0.9.0"},
		{current: "v0.9.1", target: "v0.9.0"},
		{current: "v0.9.0", target: "v1.0.0"},
		{current: "dev", target: "v0.9.0"},
		{current: "v0.8.0", target: "v0.9.0-rc.1"},
	}
	for _, test := range tests {
		if got := CanUpgrade(test.current, test.target); got != test.want {
			t.Errorf("CanUpgrade(%q, %q) = %v, want %v", test.current, test.target, got, test.want)
		}
	}
}
