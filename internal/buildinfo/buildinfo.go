// Package buildinfo reports the version of the running binary.
package buildinfo

import "runtime/debug"

// Version is set at link time (-ldflags "-X github.com/amirradjou/exitcheck/internal/buildinfo.Version=v1.2.3");
// otherwise it falls back to the module version Go records, or "dev".
var Version = ""

// String returns the best available version string.
func String() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
