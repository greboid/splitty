// Package version exposes the VCS revision the go toolchain stamps into
// binaries built from a repository checkout (see "go help buildvcs").
package version

import "runtime/debug"

// Version describes the VCS state the binary was built from.
type Version struct {
	Revision string // full commit hash
	Dirty    bool   // working tree had uncommitted changes
}

// Get returns the VCS metadata embedded in the running binary. The second
// return value is false when there is none, e.g. the binary was built via
// go run (which never stamps), outside a repository, or with
// -buildvcs=false.
func Get() (Version, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Version{}, false
	}
	var v Version
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			v.Revision = s.Value
		case "vcs.modified":
			v.Dirty = s.Value == "true"
		}
	}
	if v.Revision == "" {
		return Version{}, false
	}
	return v, true
}

// String returns a compact display form: the abbreviated commit hash,
// suffixed "-dirty" when the build captured uncommitted changes.
func (v Version) String() string {
	rev := v.Revision
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if v.Dirty {
		return rev + "-dirty"
	}
	return rev
}
