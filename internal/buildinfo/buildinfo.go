// Package buildinfo identifies the proxy executable, not its upstream client version.
package buildinfo

import (
	_ "embed"
	"fmt"
	"runtime/debug"
	"strings"
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
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if info.Version == "" {
		v := strings.TrimSpace(baseVersion)
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		info.Version = v + "-dev"
		if build != nil && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
	}
	dirty := false
	if build != nil {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				dirty = setting.Value == "true"
			case "vcs.time":
				if info.Date == "" {
					info.Date = setting.Value
				}
			}
		}
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if dirty && Commit == "" && info.Commit != "unknown" {
		info.Commit += "-dirty"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

func (i Info) String() string {
	return fmt.Sprintf("antigravity-proxy %s (commit=%s, date=%s)", i.Version, i.Commit, i.Date)
}
