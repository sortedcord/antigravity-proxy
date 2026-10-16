package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	originalVersion, originalCommit, originalDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = originalVersion, originalCommit, originalDate })
	tests := []struct {
		name     string
		injected Info
		build    *debug.BuildInfo
		want     Info
	}{
		{name: "development", want: Info{Version: "v0.1.0-dev", Commit: "unknown", Date: "unknown"}},
		{
			name: "module and vcs",
			build: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1"}, Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.time", Value: "2026-10-15T00:00:00Z"},
			}},
			want: Info{Version: "v0.1.1", Commit: "abc123", Date: "2026-10-15T00:00:00Z"},
		},
		{
			name: "injected wins", injected: Info{Version: "v0.1.0", Commit: "release", Date: "date"},
			build: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.1"}, Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "other"}, {Key: "vcs.time", Value: "other"},
			}},
			want: Info{Version: "v0.1.0", Commit: "release", Date: "date"},
		},
		{
			name: "devel module", build: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: Info{Version: "v0.1.0-dev", Commit: "unknown", Date: "unknown"},
		},
		{
			name: "dirty checkout",
			build: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"},
			}},
			want: Info{Version: "v0.1.0-dev", Commit: "abc123-dirty", Date: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version, Commit, Date = tt.injected.Version, tt.injected.Commit, tt.injected.Date
			if got := resolve(tt.build); got != tt.want {
				t.Fatalf("resolve = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	info := Info{Version: "v0.1.0", Commit: "abc123", Date: "2026-10-15T00:00:00Z"}
	if got := info.String(); got != "antigravity-proxy v0.1.0 (commit=abc123, date=2026-10-15T00:00:00Z)" {
		t.Fatalf("unexpected output: %s", got)
	}
}
