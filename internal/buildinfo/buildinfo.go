// Package buildinfo reports which build of Woolwire is running.
//
// Release builds stamp the version at link time:
//
//	go build -ldflags "-X github.com/Chrisbaack/woolwire/internal/buildinfo.version=v1.2.3" ./cmd/woolwire
//
// An unstamped build falls back to what the Go toolchain recorded: the module
// version for `go install ...@vX.Y.Z`, or the pseudo-version Go derives from
// the checkout's commit. Builds without module or VCS information (such as
// -buildvcs=false with no stamp) report the bare revision if any, else "dev".
package buildinfo

import (
	"runtime/debug"
)

// version is set with -ldflags -X. Left empty, Version derives one.
var version string

// Version returns the stamped version, else the module version, else
// "dev+<short revision>" (with "-dirty" for uncommitted changes), else "dev".
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return fromBuildInfo(info)
}

func fromBuildInfo(info *debug.BuildInfo) string {
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	v := "dev+" + revision
	if modified {
		v += "-dirty"
	}
	return v
}
