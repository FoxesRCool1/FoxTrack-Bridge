package version

import (
	"strconv"
	"strings"
)

// AppVersion is injected at build time via -ldflags.
var AppVersion = "dev"

// AppBuildVariant is injected at build time. Value is "headless" for headless builds, "" for GUI builds.
var AppBuildVariant = ""

func Normalized(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if idx := strings.IndexAny(v, "+-"); idx >= 0 {
		v = v[:idx]
	}
	if v == "" {
		return "0.0.0"
	}
	return v
}

// IsValid reports whether v parses as a numeric x.y.z version. AppVersion
// defaults to "dev" for source builds, which is not a valid version; this
// lets callers skip update checks instead of comparing it as 0.0.0.
func IsValid(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	for _, p := range strings.Split(Normalized(v), ".") {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

func Compare(a, b string) int {
	ap := parseParts(Normalized(a))
	bp := parseParts(Normalized(b))
	for i := 0; i < 3; i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	return 0
}

func parseParts(v string) [3]int {
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(out) && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err == nil && n >= 0 {
			out[i] = n
		}
	}
	return out
}
