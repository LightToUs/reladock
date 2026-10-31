package buildinfo

import (
	"regexp"
	"strconv"
)

var ReleaseVersion = "dev"

var productionVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func Current() string {
	if ReleaseVersion == "dev" {
		return "dev"
	}
	if _, _, _, ok := ParseProduction(ReleaseVersion); ok {
		return ReleaseVersion
	}
	return "dev"
}

func ParseProduction(value string) (major, minor, patch int, ok bool) {
	parts := productionVersion.FindStringSubmatch(value)
	if parts == nil {
		return 0, 0, 0, false
	}
	values := []*int{&major, &minor, &patch}
	for i := range values {
		parsed, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return 0, 0, 0, false
		}
		*values[i] = parsed
	}
	return major, minor, patch, true
}

func CanUpgrade(current, target string) bool {
	currentMajor, currentMinor, currentPatch, currentOK := ParseProduction(current)
	targetMajor, targetMinor, targetPatch, targetOK := ParseProduction(target)
	if !currentOK || !targetOK || currentMajor != targetMajor {
		return false
	}
	if targetMinor != currentMinor {
		return targetMinor > currentMinor
	}
	return targetPatch > currentPatch
}
