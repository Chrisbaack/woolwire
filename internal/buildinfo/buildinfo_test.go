package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{
			name: "module version from go install",
			info: debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}},
			want: "v1.2.3",
		},
		{
			name: "checkout build uses the revision",
			info: debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "639c92a0123456789abcdef"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			want: "dev+639c92a01234",
		},
		{
			name: "uncommitted changes are marked",
			info: debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "639c92a"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: "dev+639c92a-dirty",
		},
		{
			name: "no version control information",
			info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: "dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fromBuildInfo(&tt.info); got != tt.want {
				t.Errorf("fromBuildInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}
