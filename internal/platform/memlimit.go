package platform

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// MemoryCeiling reports how much memory this process may reasonably assume it
// has, and where that number came from.
//
// It exists for one caller: sizing the password-hashing gate. argon2id is
// configured to allocate 19 MiB per hash (OWASP), so "how many may run at once"
// is a memory question, and answering it with a hardcoded number is how a 2 GiB
// container and a 64 GiB host end up with the same limit — one of them wrong.
//
// Sources, in order of authority:
//
//  1. GOMEMLIMIT / debug.SetMemoryLimit — if an operator has stated a ceiling,
//     that IS the ceiling, and nothing else gets a vote.
//  2. cgroup v2 memory.max, then v1 memory.limit_in_bytes — the container's
//     limit, which is what actually kills the process.
//  3. /proc/meminfo MemAvailable — the host's usable memory.
//
// Everything after (1) is Linux. On Windows and macOS this returns (0, "unknown")
// and the caller falls back to its configured default, which is the honest
// outcome: guessing at a ceiling is worse than admitting there is no reading.
func MemoryCeiling() (bytes uint64, source string) {
	// A Go memory limit is authoritative: exceeding it makes the collector
	// thrash long before the OS gets involved.
	if lim := debug.SetMemoryLimit(-1); lim > 0 && lim != maxInt64 {
		return uint64(lim), "GOMEMLIMIT"
	}
	if v, ok := readCgroupLimit("/sys/fs/cgroup/memory.max"); ok {
		return v, "cgroup v2 memory.max"
	}
	if v, ok := readCgroupLimit("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok {
		return v, "cgroup v1 memory.limit_in_bytes"
	}
	if v, ok := readMemAvailable("/proc/meminfo"); ok {
		return v, "/proc/meminfo MemAvailable"
	}
	return 0, "unknown"
}

// maxInt64 is what debug.SetMemoryLimit reports when no limit is set.
const maxInt64 = 1<<63 - 1

// readCgroupLimit reads a cgroup memory limit file. "max" (v2) and the
// enormous sentinel v1 uses both mean "no limit", which is not a reading.
func readCgroupLimit(path string) (uint64, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed cgroup paths, not user input
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	// cgroup v1 signals "unlimited" with a value near 2^63; anything at or above
	// a petabyte is that sentinel, not a real limit.
	if v >= 1<<50 {
		return 0, false
	}
	return v, true
}

// readMemAvailable pulls MemAvailable (in KiB) out of /proc/meminfo.
func readMemAvailable(path string) (uint64, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed procfs path, not user input
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return 0, false
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kib * 1024, true
	}
	return 0, false
}
