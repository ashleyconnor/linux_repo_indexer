package repoconfig

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
)

// ErrNotIndexed is returned by Classify for a key that is not a package in any
// configured repository — an index file, a stray upload, or a package under a
// prefix no longer in the config. Ingest logs and skips these rather than
// failing, so an unrelated object in the bucket cannot wedge the queue.
var ErrNotIndexed = errors.New("repoconfig: key is not an indexed package")

// A Config is the whole repository definition, loaded from repos.yaml.
type Config struct {
	Version int     `yaml:"version"`
	Signing Signing `yaml:"signing"`
	APT     *APT    `yaml:"apt"`
	RPM     *RPM    `yaml:"rpm"`
}

// Signing names the Secrets Manager secret holding the OpenPGP private key.
// KMS cannot do OpenPGP, so the key is a secret rather than a KMS key.
type Signing struct {
	SecretID string `yaml:"secret_id"`
}

// APT describes the Debian side of the repository.
type APT struct {
	Origin      string `yaml:"origin"`
	Label       string `yaml:"label"`
	Description string `yaml:"description"`

	// PoolPrefix is where packages live: <pool_prefix>/<arch>/<component>/.
	// Note that architecture comes before component, which is the layout the
	// live repository uses.
	PoolPrefix string `yaml:"pool_prefix"`

	// DistsPrefix is where indexes are published: <dists_prefix>/<codename>/.
	DistsPrefix string `yaml:"dists_prefix"`

	AcquireByHash bool                `yaml:"acquire_by_hash"`
	Compressions  []index.Compression `yaml:"compressions"`

	Components    []string `yaml:"components"`
	Architectures []string `yaml:"architectures"`
	Codenames     []string `yaml:"codenames"`
}

// RPM describes the yum side of the repository.
type RPM struct {
	Trees []Tree `yaml:"trees"`
}

// A Tree is one self-contained yum repository at
// <distro>/<version>/<arch>/<channel>/.
type Tree struct {
	Distro        string   `yaml:"distro"`
	Version       string   `yaml:"version"`
	Architectures []string `yaml:"architectures"`
	Channels      []string `yaml:"channels"`
}

// Defaults applied when repos.yaml leaves a field out.
const (
	defaultPoolPrefix  = "pool"
	defaultDistsPrefix = "dists"
)

// Parse reads and validates a repos.yaml document.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a typo in a key is a config error, not a silent default
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("repoconfig: parsing repos.yaml: %w", err)
	}

	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.APT == nil {
		return
	}
	if c.APT.PoolPrefix == "" {
		c.APT.PoolPrefix = defaultPoolPrefix
	}
	if c.APT.DistsPrefix == "" {
		c.APT.DistsPrefix = defaultDistsPrefix
	}
}

// Validate rejects a configuration that would publish a broken repository.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("repoconfig: unsupported version %d, want 1", c.Version)
	}
	if c.APT == nil && c.RPM == nil {
		return errors.New("repoconfig: no apt or rpm section, nothing to publish")
	}

	if a := c.APT; a != nil {
		if len(a.Components) == 0 {
			return errors.New("repoconfig: apt has no components")
		}
		if len(a.Architectures) == 0 {
			return errors.New("repoconfig: apt has no architectures")
		}
		if len(a.Codenames) == 0 {
			return errors.New("repoconfig: apt has no codenames")
		}
		if strings.Contains(a.PoolPrefix, "/") || strings.Contains(a.DistsPrefix, "/") {
			return errors.New("repoconfig: apt pool_prefix and dists_prefix must be single path segments")
		}
		if a.PoolPrefix == a.DistsPrefix {
			return errors.New("repoconfig: apt pool_prefix and dists_prefix must differ")
		}
		for _, comp := range a.Compressions {
			switch comp {
			case index.CompressionGzip, index.CompressionXZ:
			default:
				return fmt.Errorf("repoconfig: unsupported apt compression %q", comp)
			}
		}
		if err := noEmpty("apt components", a.Components); err != nil {
			return err
		}
		if err := noEmpty("apt architectures", a.Architectures); err != nil {
			return err
		}
		if err := noEmpty("apt codenames", a.Codenames); err != nil {
			return err
		}
	}

	if r := c.RPM; r != nil {
		if len(r.Trees) == 0 {
			return errors.New("repoconfig: rpm section has no trees")
		}
		seen := make(map[string]bool)
		for i, t := range r.Trees {
			if t.Distro == "" || t.Version == "" {
				return fmt.Errorf("repoconfig: rpm tree %d has no distro or version", i)
			}
			if len(t.Architectures) == 0 || len(t.Channels) == 0 {
				return fmt.Errorf("repoconfig: rpm tree %s/%s has no architectures or channels", t.Distro, t.Version)
			}
			key := t.Distro + "/" + t.Version
			if seen[key] {
				return fmt.Errorf("repoconfig: rpm tree %s is defined twice", key)
			}
			seen[key] = true

			// A distro name that collides with the apt prefixes would make key
			// classification ambiguous.
			if c.APT != nil && (t.Distro == c.APT.PoolPrefix || t.Distro == c.APT.DistsPrefix) {
				return fmt.Errorf("repoconfig: rpm distro %q collides with an apt prefix", t.Distro)
			}
		}
	}

	return nil
}

func noEmpty(what string, values []string) error {
	if slices.Contains(values, "") {
		return fmt.Errorf("repoconfig: %s contains an empty entry", what)
	}
	return nil
}

// Classify maps an S3 object key to the storage scope that owns it and the
// path the index should record for it.
//
// The returned filename is what goes in the index: for Debian that is the key
// itself, because apt resolves Filename against the repository root; for RPM
// it is the base name, because dnf resolves location href against the tree.
func (c *Config) Classify(key string) (Scope, string, error) {
	key = strings.TrimPrefix(key, "/")
	parts := strings.Split(key, "/")

	switch {
	case c.APT != nil && strings.HasSuffix(key, ".deb") && parts[0] == c.APT.PoolPrefix:
		// pool/<arch>/<component>/<file>.deb
		if len(parts) != 4 {
			return Scope{}, "", fmt.Errorf("%w: %q is under the pool but not pool/<arch>/<component>/<file>.deb", ErrNotIndexed, key)
		}
		arch, component := parts[1], parts[2]
		if !slices.Contains(c.APT.Architectures, arch) {
			return Scope{}, "", fmt.Errorf("%w: architecture %q is not configured", ErrNotIndexed, arch)
		}
		if !slices.Contains(c.APT.Components, component) {
			return Scope{}, "", fmt.Errorf("%w: component %q is not configured", ErrNotIndexed, component)
		}
		return DebPoolScope(component, arch), key, nil

	case c.RPM != nil && strings.HasSuffix(key, ".rpm"):
		// <distro>/<version>/<arch>/<channel>/<file>.rpm
		if len(parts) != 5 {
			return Scope{}, "", fmt.Errorf("%w: %q is not <distro>/<version>/<arch>/<channel>/<file>.rpm", ErrNotIndexed, key)
		}
		distro, version, arch, channel := parts[0], parts[1], parts[2], parts[3]
		tree := c.tree(distro, version)
		if tree == nil {
			return Scope{}, "", fmt.Errorf("%w: no rpm tree %s/%s", ErrNotIndexed, distro, version)
		}
		if !slices.Contains(tree.Architectures, arch) {
			return Scope{}, "", fmt.Errorf("%w: %s/%s has no architecture %q", ErrNotIndexed, distro, version, arch)
		}
		if !slices.Contains(tree.Channels, channel) {
			return Scope{}, "", fmt.Errorf("%w: %s/%s has no channel %q", ErrNotIndexed, distro, version, channel)
		}
		return RPMTreeScope(distro, version, arch, channel), parts[4], nil

	default:
		return Scope{}, "", fmt.Errorf("%w: %q", ErrNotIndexed, key)
	}
}

func (c *Config) tree(distro, version string) *Tree {
	if c.RPM == nil {
		return nil
	}
	for i := range c.RPM.Trees {
		if c.RPM.Trees[i].Distro == distro && c.RPM.Trees[i].Version == version {
			return &c.RPM.Trees[i]
		}
	}
	return nil
}

// PublishScopes returns every scope that should currently be published. The
// publisher's scheduled sweep uses it to find scopes that config has newly
// added, and the seeder uses it to know what to fill.
func (c *Config) PublishScopes() []Scope {
	var out []Scope
	if a := c.APT; a != nil {
		for _, comp := range a.Components {
			for _, arch := range a.Architectures {
				out = append(out, DebPoolScope(comp, arch))
			}
		}
		for _, codename := range a.Codenames {
			out = append(out, DebReleaseScope(codename))
		}
	}
	if r := c.RPM; r != nil {
		for _, t := range r.Trees {
			for _, arch := range t.Architectures {
				for _, channel := range t.Channels {
					out = append(out, RPMTreeScope(t.Distro, t.Version, arch, channel))
				}
			}
		}
	}
	return out
}

// Knows reports whether a scope is still configured. A scope that has been
// removed from repos.yaml stops being published, but its published tree is
// left alone until someone retires it explicitly.
func (c *Config) Knows(s Scope) bool {
	if s.hasEmptyPart() {
		return false
	}
	switch s.Kind {
	case KindDebPool:
		return c.APT != nil &&
			slices.Contains(c.APT.Components, s.Component) &&
			slices.Contains(c.APT.Architectures, s.Architecture)
	case KindDebRelease:
		return c.APT != nil && slices.Contains(c.APT.Codenames, s.Codename)
	case KindRPMTree:
		t := c.tree(s.Distro, s.Version)
		return t != nil &&
			slices.Contains(t.Architectures, s.Architecture) &&
			slices.Contains(t.Channels, s.Channel)
	default:
		return false
	}
}

// ReleaseScopesFor returns the codename Release files affected by a change to
// a pool scope. This is the fan-out that makes one uploaded .deb appear in
// every codename's index.
func (c *Config) ReleaseScopesFor(pool Scope) []Scope {
	if c.APT == nil || pool.Kind != KindDebPool || !c.Knows(pool) {
		return nil
	}
	out := make([]Scope, 0, len(c.APT.Codenames))
	for _, codename := range c.APT.Codenames {
		out = append(out, DebReleaseScope(codename))
	}
	return out
}

// PoolScopesFor returns every pool scope whose index files a codename's
// Release must cover.
func (c *Config) PoolScopesFor(release Scope) []Scope {
	if c.APT == nil || release.Kind != KindDebRelease || !c.Knows(release) {
		return nil
	}
	var out []Scope
	for _, comp := range c.APT.Components {
		for _, arch := range c.APT.Architectures {
			out = append(out, DebPoolScope(comp, arch))
		}
	}
	return out
}

// IndexDir returns where a pool scope's index files are published under a
// codename, relative to the repository root.
func (c *Config) IndexDir(codename string, pool Scope) string {
	return path.Join(c.APT.DistsPrefix, codename, pool.Component, "binary-"+pool.Architecture)
}

// IndexDirRelativeToCodename returns the same location as the Release file
// references it: relative to the codename directory.
func (c *Config) IndexDirRelativeToCodename(pool Scope) string {
	return path.Join(pool.Component, "binary-"+pool.Architecture)
}

// CodenameDir returns a codename's directory, where Release, Release.gpg and
// InRelease are published.
func (c *Config) CodenameDir(codename string) string {
	return path.Join(c.APT.DistsPrefix, codename)
}

// TreeDir returns a yum tree's root directory, which is what a client's
// baseurl points at.
func (c *Config) TreeDir(s Scope) string {
	return path.Join(s.Distro, s.Version, s.Architecture, s.Channel)
}
