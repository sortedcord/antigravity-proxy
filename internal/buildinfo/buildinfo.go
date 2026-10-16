// Package buildinfo identifies the proxy executable, not its upstream client version.
package buildinfo

import (
	_ "embed"
	"runtime/debug"

	libbuild "github.com/terastal-hq/proxylib/buildinfo"
)

//go:embed VERSION
var baseVersion string

// Release tooling sets these strings with -ldflags -X.
var (
	Version string
	Commit  string
	Date    string
)

// Info is non-secret executable identity.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Current returns injected metadata with Go build information as a fallback.
func Current() Info {
	info, _ := debug.ReadBuildInfo()
	return resolve(info)
}

func resolve(build *debug.BuildInfo) Info {
	res := libbuild.Resolve(baseVersion, Version, Commit, Date, build)
	return Info{
		Version: res.Version,
		Commit:  res.Commit,
		Date:    res.Date,
	}
}

func (i Info) String() string {
	return libbuild.Info{Version: i.Version, Commit: i.Commit, Date: i.Date}.Format("antigravity-proxy")
}
