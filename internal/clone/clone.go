// Package clone carries a yum tree's packages forward into a new distro
// version.
//
// A yum tree is self-contained: nothing is shared between RHEL/9 and RHEL/10,
// so a new version publishes empty metadata until the packages exist under its
// own prefix. The apt side has no equivalent problem — a new codename publishes
// the packages already in the shared pool — which is why this only deals in
// rpm trees.
package clone

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// A Version names one distro version: the <distro>/<version> half of a tree
// path, without the architecture and channel that complete it.
type Version struct {
	Distro  string
	Version string
}

func (v Version) String() string { return path.Join(v.Distro, v.Version) }

// ParseVersion reads a <distro>/<version> pair.
func ParseVersion(s string) (Version, error) {
	trimmed := strings.Trim(s, "/")

	// A deb scope is the likeliest wrong input, so it is worth diagnosing
	// rather than reporting as a malformed version.
	if head, _, _ := strings.Cut(trimmed, "/"); head == string(repoconfig.KindDebPool) || head == string(repoconfig.KindDebRelease) {
		return Version{}, fmt.Errorf(
			"clone: %q is a deb scope; the deb pool is shared across codenames, so a new codename already publishes every package and needs no copying", s)
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Version{}, fmt.Errorf("clone: %q is not <distro>/<version>, for example RHEL/9", s)
	}
	return Version{Distro: parts[0], Version: parts[1]}, nil
}

// A TreePair is one tree's worth of work: where the packages come from and
// where they are going.
type TreePair struct {
	From, To       repoconfig.Scope
	FromDir, ToDir string
}

// Trees expands a clone into the individual trees it touches.
//
// The target's configuration drives the matrix, not the source's. An
// architecture the new version adds and the old one never had simply finds no
// packages to copy, and the source version does not need to be in repos.yaml at
// all — a version already retired from the config still has its objects, and
// cloning from it is legitimate.
func Trees(cfg *repoconfig.Config, from, to Version) ([]TreePair, error) {
	if from == to {
		return nil, fmt.Errorf("clone: %s is both the source and the target", from)
	}

	tree := findTree(cfg, to)
	if tree == nil {
		return nil, fmt.Errorf(
			"clone: %s is not in repos.yaml; add the tree there first, or the copied packages would land somewhere nothing publishes", to)
	}

	var pairs []TreePair
	for _, arch := range tree.Architectures {
		for _, channel := range tree.Channels {
			src := repoconfig.RPMTreeScope(from.Distro, from.Version, arch, channel)
			dst := repoconfig.RPMTreeScope(to.Distro, to.Version, arch, channel)
			pairs = append(pairs, TreePair{
				From:    src,
				To:      dst,
				FromDir: cfg.TreeDir(src),
				ToDir:   cfg.TreeDir(dst),
			})
		}
	}
	return pairs, nil
}

func findTree(cfg *repoconfig.Config, v Version) *repoconfig.Tree {
	if cfg.RPM == nil {
		return nil
	}
	for i := range cfg.RPM.Trees {
		if cfg.RPM.Trees[i].Distro == v.Distro && cfg.RPM.Trees[i].Version == v.Version {
			return &cfg.RPM.Trees[i]
		}
	}
	return nil
}

// A Copy is one object moving from one key to another.
type Copy struct{ From, To string }

// A TreeCopy is what one tree needs, once both sides have been listed.
type TreeCopy struct {
	TreePair
	Copies []Copy

	// Note explains an empty Copies. "Nothing to do" has two causes and an
	// operator expecting packages needs to know which one happened.
	Note string
}

// Plan decides what to copy for one tree.
func Plan(pair TreePair, source, target []string) TreeCopy {
	out := TreeCopy{TreePair: pair}

	present := make(map[string]bool, len(target))
	for _, key := range target {
		present[path.Base(key)] = true
	}

	var packages int
	for _, key := range source {
		// Filtering on the suffix is what keeps repodata, checksums and any
		// other accumulated debris out of the new tree.
		if !strings.HasSuffix(key, ".rpm") {
			continue
		}
		packages++

		name := path.Base(key)
		if present[name] {
			continue
		}
		out.Copies = append(out.Copies, Copy{From: key, To: path.Join(pair.ToDir, name)})
	}

	switch {
	case len(out.Copies) > 0:
	case packages == 0:
		out.Note = "source is empty"
	default:
		out.Note = fmt.Sprintf("all %d already present", packages)
	}
	return out
}

// A Copier is the object storage this needs: enough to see what is there and
// to copy it.
type Copier interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Copy(ctx context.Context, srcKey, dstKey string) error
}

// PlanAll lists both sides of every pair and plans the work.
func PlanAll(ctx context.Context, c Copier, pairs []TreePair) ([]TreeCopy, error) {
	plans := make([]TreeCopy, 0, len(pairs))
	for _, pair := range pairs {
		source, err := c.List(ctx, pair.FromDir+"/")
		if err != nil {
			return nil, fmt.Errorf("clone: listing %s: %w", pair.FromDir, err)
		}
		target, err := c.List(ctx, pair.ToDir+"/")
		if err != nil {
			return nil, fmt.Errorf("clone: listing %s: %w", pair.ToDir, err)
		}
		plans = append(plans, Plan(pair, source, target))
	}
	return plans, nil
}

// Execute performs the copies and returns how many objects were copied.
//
// It stops at the first failure rather than continuing: a partial clone that
// reported success would be published as though it were complete.
func Execute(ctx context.Context, c Copier, plans []TreeCopy) (int, error) {
	var copied int
	for _, plan := range plans {
		for _, cp := range plan.Copies {
			if err := c.Copy(ctx, cp.From, cp.To); err != nil {
				return copied, fmt.Errorf("clone: copying %s: %w", cp.From, err)
			}
			copied++
		}
	}
	return copied, nil
}
