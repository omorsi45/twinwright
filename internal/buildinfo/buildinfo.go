// Package buildinfo describes the binary that produced an artifact, so two
// artifacts can be compared knowing what built each of them.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Info identifies one build of Twinwright.
type Info struct {
	Version  string `json:"twinwright_version"`
	Revision string `json:"git_revision,omitempty"`
	// Modified reports whether the working tree had uncommitted changes when the
	// binary was built, because a revision alone does not identify such a build.
	Modified  bool   `json:"git_modified"`
	GoVersion string `json:"go_version"`
}

// Read describes the running binary. version is the linker-supplied version
// (cmd/twinwright's Version); when it is empty Read falls back to the module
// version the toolchain stamped in, then to "dev", so a go install build still
// identifies itself.
//
// Revision stays empty when the toolchain recorded no VCS settings. That is
// normal for go test and go run binaries and for builds outside a checkout, so
// it is not an error.
func Read(version string) Info {
	info := Info{Version: version, GoVersion: runtime.Version()}
	build, ok := debug.ReadBuildInfo()
	if ok {
		if info.Version == "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.Revision = setting.Value
			case "vcs.modified":
				info.Modified = setting.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}
