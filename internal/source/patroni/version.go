package patroni

import (
	"strconv"
	"strings"
)

// MinVersion is the oldest Patroni the operations rely on: demote-cluster and a clean
// demotion on standby_cluster appear in 4.1.0.
const MinVersion = "4.1.0"

var minVersion = [3]int{4, 1, 0}

// VersionSupported reports whether version is [MinVersion] or newer. A suffix such as
// ".dev1" is ignored; a version that cannot be parsed is not supported.
func VersionSupported(version string) bool {
	v, ok := parseVersion(version)
	if !ok {
		return false
	}

	for i := range v {
		if v[i] != minVersion[i] {
			return v[i] > minVersion[i]
		}
	}

	return true
}

// parseVersion reads major.minor[.patch[...]]; the missing patch is 0.
func parseVersion(s string) ([3]int, bool) {
	var v [3]int

	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 2 { //nolint:mnd // major and minor are required
		return v, false
	}

	for i := range v {
		if i >= len(parts) {
			break
		}

		digits := strings.TrimRight(parts[i], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ+-_")

		n, err := strconv.Atoi(digits)
		if err != nil || n < 0 {
			return v, false
		}

		v[i] = n
	}

	return v, true
}
