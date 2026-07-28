// Package version carries build-time identity for the nimbus binary.
package version

import (
	"fmt"
	"runtime"
)

// Populated via -ldflags at build time; see scripts/build.sh.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the full version banner.
func String() string {
	return fmt.Sprintf("nimbus %s (%s) built %s %s/%s %s",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Platform returns the GOOS/GOARCH pair this binary was built for.
func Platform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
