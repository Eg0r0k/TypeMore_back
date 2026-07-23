package platform

import "runtime"

// Build metadata. These are overridden at link time via -ldflags -X (see the
// Makefile and Dockerfile); the defaults keep `go run` output sensible.
//
//	go build -ldflags "-X github.com/typemore/typemore-server/internal/platform.Version=1.2.3"
var (
	// Version is the release/tag of this build.
	Version = "dev"
	// Commit is the git revision this build was cut from.
	Commit = "none"
	// BuildDate is the UTC build timestamp.
	BuildDate = "unknown"
)

// BuildInfo is the machine-readable snapshot of build metadata returned by the
// health endpoint. It is a plain value so callers can serialize or log it.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// CurrentBuild returns the build metadata for this binary.
func CurrentBuild() BuildInfo {
	return BuildInfo{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
	}
}
