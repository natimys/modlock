package updater

import (
	"fmt"
	"strconv"
	"strings"
)

type semver struct {
	major, minor, patch int
	prerelease          string
}

func parseVersion(raw string) (semver, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	mainAndPre, metadata, hasMetadata := strings.Cut(raw, "+")
	if hasMetadata && !validIdentifiers(metadata, false) {
		return semver{}, fmt.Errorf("invalid SemVer %q", raw)
	}
	main, pre, hasPre := strings.Cut(mainAndPre, "-")
	core := strings.Split(main, ".")
	if len(core) != 3 {
		return semver{}, fmt.Errorf("invalid SemVer %q", raw)
	}
	values := [3]int{}
	for i, part := range core {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, fmt.Errorf("invalid SemVer %q", raw)
		}
		v, err := strconv.Atoi(part)
		if err != nil || v < 0 {
			return semver{}, fmt.Errorf("invalid SemVer %q", raw)
		}
		values[i] = v
	}
	if hasPre && !validIdentifiers(pre, true) {
		return semver{}, fmt.Errorf("invalid SemVer %q", raw)
	}
	return semver{values[0], values[1], values[2], pre}, nil
}

// validateVersionPath accepts only canonical directory names. Version
// comparison remains permissive of a leading v and surrounding whitespace,
// but pointer values are used directly in filesystem paths.
func validateVersionPath(raw string) error {
	if raw != strings.TrimSpace(raw) || strings.HasPrefix(raw, "v") {
		return fmt.Errorf("invalid version directory %q", raw)
	}
	_, err := parseVersion(raw)
	return err
}

func validIdentifiers(value string, prerelease bool) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		numeric := true
		for _, r := range part {
			if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '-') {
				return false
			}
			if r < '0' || r > '9' {
				numeric = false
			}
		}
		if prerelease && numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func compareVersions(a, b string) (int, error) {
	x, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	y, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for _, pair := range [][2]int{{x.major, y.major}, {x.minor, y.minor}, {x.patch, y.patch}} {
		if pair[0] < pair[1] {
			return -1, nil
		}
		if pair[0] > pair[1] {
			return 1, nil
		}
	}
	if x.prerelease == y.prerelease {
		return 0, nil
	}
	if x.prerelease == "" {
		return 1, nil
	}
	if y.prerelease == "" {
		return -1, nil
	}
	xp, yp := strings.Split(x.prerelease, "."), strings.Split(y.prerelease, ".")
	for i := 0; i < len(xp) && i < len(yp); i++ {
		if xp[i] == yp[i] {
			continue
		}
		xNumeric, yNumeric := numericIdentifier(xp[i]), numericIdentifier(yp[i])
		if xNumeric && yNumeric {
			if len(xp[i]) < len(yp[i]) || len(xp[i]) == len(yp[i]) && xp[i] < yp[i] {
				return -1, nil
			}
			return 1, nil
		}
		if xNumeric {
			return -1, nil
		}
		if yNumeric {
			return 1, nil
		}
		if xp[i] < yp[i] {
			return -1, nil
		}
		return 1, nil
	}
	if len(xp) < len(yp) {
		return -1, nil
	}
	return 1, nil
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
