// Package repoconfig defines which repositories exist and maps S3 object keys
// onto them.
//
// The set of distributions is data, not code: adding a codename or a yum tree
// is an edit to repos.yaml in the bucket, and the next publish picks it up.
// Removing one stops publishing but never deletes the published tree, so a
// typo in the config cannot destroy a live repository.
package repoconfig

import (
	"fmt"
	"strings"
)

// Kind distinguishes the three things a scope can name.
type Kind string

const (
	// KindDebPool is a set of .deb packages sharing a component and
	// architecture. It is deliberately codename-independent: the live pool is
	// shared, and the same package set is published to every codename.
	KindDebPool Kind = "deb"

	// KindDebRelease is one codename's Release file. It exists as a separate
	// scope because a Release spans every component and architecture, so it
	// cannot be built from a single pool scope.
	KindDebRelease Kind = "release"

	// KindRPMTree is one self-contained yum tree.
	KindRPMTree Kind = "rpm"
)

// A Scope names a unit of work: a set of packages that are stored together and
// an index that is published together.
type Scope struct {
	Kind Kind

	Component    string // deb pool
	Architecture string // deb pool, rpm tree
	Codename     string // deb release
	Distro       string // rpm tree
	Version      string // rpm tree
	Channel      string // rpm tree
}

// DebPoolScope returns the storage scope for a component and architecture.
func DebPoolScope(component, arch string) Scope {
	return Scope{Kind: KindDebPool, Component: component, Architecture: arch}
}

// DebReleaseScope returns the publish scope for one codename's Release file.
func DebReleaseScope(codename string) Scope {
	return Scope{Kind: KindDebRelease, Codename: codename}
}

// RPMTreeScope returns the scope for one yum tree.
func RPMTreeScope(distro, version, arch, channel string) Scope {
	return Scope{
		Kind:         KindRPMTree,
		Distro:       distro,
		Version:      version,
		Architecture: arch,
		Channel:      channel,
	}
}

// String renders the scope as the key used for DynamoDB partitions, queue
// messages and log lines.
func (s Scope) String() string {
	switch s.Kind {
	case KindDebPool:
		return strings.Join([]string{string(KindDebPool), s.Component, s.Architecture}, "/")
	case KindDebRelease:
		return strings.Join([]string{string(KindDebRelease), s.Codename}, "/")
	case KindRPMTree:
		return strings.Join([]string{string(KindRPMTree), s.Distro, s.Version, s.Architecture, s.Channel}, "/")
	default:
		return string(s.Kind)
	}
}

// IsZero reports whether the scope is unset.
func (s Scope) IsZero() bool { return s.Kind == "" }

// ParseScope is the inverse of String. Publish messages carry scopes as
// strings, so they have to survive the round trip.
func ParseScope(s string) (Scope, error) {
	parts := strings.Split(s, "/")
	if len(parts) == 0 {
		return Scope{}, fmt.Errorf("repoconfig: empty scope")
	}

	switch Kind(parts[0]) {
	case KindDebPool:
		if len(parts) != 3 {
			return Scope{}, fmt.Errorf("repoconfig: malformed deb pool scope %q, want deb/<component>/<arch>", s)
		}
		return DebPoolScope(parts[1], parts[2]), nil

	case KindDebRelease:
		if len(parts) != 2 {
			return Scope{}, fmt.Errorf("repoconfig: malformed release scope %q, want release/<codename>", s)
		}
		return DebReleaseScope(parts[1]), nil

	case KindRPMTree:
		if len(parts) != 5 {
			return Scope{}, fmt.Errorf("repoconfig: malformed rpm scope %q, want rpm/<distro>/<version>/<arch>/<channel>", s)
		}
		return RPMTreeScope(parts[1], parts[2], parts[3], parts[4]), nil

	default:
		return Scope{}, fmt.Errorf("repoconfig: unknown scope kind in %q", s)
	}
}

// hasEmptyPart reports whether any component of the scope that should be set
// is blank, which would produce an ambiguous key.
func (s Scope) hasEmptyPart() bool {
	switch s.Kind {
	case KindDebPool:
		return s.Component == "" || s.Architecture == ""
	case KindDebRelease:
		return s.Codename == ""
	case KindRPMTree:
		return s.Distro == "" || s.Version == "" || s.Architecture == "" || s.Channel == ""
	default:
		return true
	}
}
