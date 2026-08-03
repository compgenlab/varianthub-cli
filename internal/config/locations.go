package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Locations records where one source's data was actually provisioned.
//
// A source manifest describes what a source *is* — url, format, annotations —
// and must stay portable enough to share through a registry. Where a copy
// happens to live is a property of one machine. Keeping the two apart is what
// lets a single annotation run read one source from local disk, another from
// S3 and a third over HTTP: cache_dir names one place, and searching several
// would cost a round trip per source per place on every run.
//
// It is written per source, beside the manifest it overlays, rather than as one
// file for the whole home. That is not just tidiness: two `varhub download
// --source` runs provisioning different sources concurrently would otherwise
// race on a single shared file and one would lose its entry, leaving data in
// place that nothing knows about. Separate files cannot collide.
type Locations struct {
	// Root replaces cache_dir for this source: its files resolve under here by
	// the usual <name>/<version>/<basename> convention.
	//
	// This is the common case and the reason the overlay is worth having. A
	// deployment that provisioned a source to a bucket knows one fact — which
	// bucket — and per-file entries would make it restate the layout it did not
	// choose. Multi-file sources follow automatically, since the convention
	// still applies underneath.
	Root string `toml:"root,omitempty"`

	// GTFIndex is the bgzipped+indexed copy a GTF source is queried through.
	// Recorded separately because it is not one of the downloaded files:
	// varhub converts the original and reads the result, so recording only the
	// download would point annotation at the wrong file.
	GTFIndex string `toml:"gtf_index,omitempty"`

	Files []FileLocation `toml:"file"`

	// ToolCache is where a tool's installed data is archived so another machine
	// can unpack it instead of repeating the install, e.g. "s3://bucket/prefix".
	//
	// One field rather than a flag plus a hidden destination: an empty value
	// means off, and there is no way to be switched on with nowhere to write.
	// It can also name a different bucket from the one holding source data,
	// which a flag inheriting cache_dir could not express.
	//
	// A deployment fact, not a source one, which is why it lives here rather
	// than in the manifest. Whether the machine that runs a tool's setup is the
	// machine that later uses it depends on how something is deployed, and a
	// manifest shared through a registry would be asserting it for everyone.
	ToolCache string `toml:"tool_cache,omitempty"`

	// AnnotationPrefix renames this source's output fields for this deployment,
	// without a snapshot having to say so.
	//
	// A catalog may hold two versions of one source and want them permanently
	// distinguishable, whatever pins them — so this is the deployment's standing
	// answer, and a snapshot that needs something different overrides it. Set to
	// "-" for no prefix, since an empty string means "not set".
	AnnotationPrefix string `toml:"annotation_prefix,omitempty"`

	path string // where it was loaded from, for writing back
}

// FileLocation is where one of a source's files ended up. Chrom and Alt
// identify which file, for sources that resolve to several; both are empty for
// the single-file case.
type FileLocation struct {
	Chrom string `toml:"chrom,omitempty"`
	Alt   string `toml:"alt,omitempty"`
	Path  string `toml:"path"`
	Index string `toml:"index,omitempty"`
}

// LocationsPath is the overlay beside a source's manifest.
func (c *Config) LocationsPath(name, version string) string {
	return filepath.Join(c.SourceDir(name, version), name+"-"+version+".locations.toml")
}

// NewLocations prepares an overlay for writing to a source's directory.
func (c *Config) NewLocations(name, version string, l *Locations) *Locations {
	l.path = c.LocationsPath(name, version)
	return l
}

// LoadLocations reads a source's overlay. A missing file is not an error: a
// source resolved by the cache_dir convention needs no overlay at all.
func (c *Config) LoadLocations(name, version string) (*Locations, error) {
	p := c.LocationsPath(name, version)
	l := &Locations{path: p}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	if err := toml.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	l.path = p
	for i, f := range l.Files {
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("%s: [[file]] %d has no path", p, i+1)
		}
	}
	return l, nil
}

// AnnotationPrefixOverride returns this deployment's prefix for the source, or
// "" when it has not set one.
func (l *Locations) AnnotationPrefixOverride() string {
	if l == nil {
		return ""
	}
	return l.AnnotationPrefix
}

// ToolCacheRoot is where this source's installed tool data should be archived,
// or "" when it should not be.
func (l *Locations) ToolCacheRoot() string {
	if l == nil {
		return ""
	}
	return l.ToolCache
}

// Empty reports whether the overlay records nothing, in which case resolution
// falls back to the cache_dir convention.
//
// CacheSetup is deliberately not counted: it says nothing about where files are,
// so an overlay carrying only that must still leave resolution to the
// convention rather than looking like it overrides something.
func (l *Locations) Empty() bool {
	return l == nil || (l.Root == "" && l.GTFIndex == "" && len(l.Files) == 0)
}

// File returns the recorded location for one file of a source, matched by the
// chrom/alt key that identifies it.
func (l *Locations) File(chrom, alt string) (FileLocation, bool) {
	if l == nil {
		return FileLocation{}, false
	}
	for _, f := range l.Files {
		if f.Chrom == chrom && f.Alt == alt {
			return f, true
		}
	}
	return FileLocation{}, false
}

// Save writes the overlay, atomically.
//
// Replace rather than merge: a re-provision that yields fewer files — a source
// dropping a per-chromosome split — must not leave the old ones recorded as
// present.
func (l *Locations) Save() error {
	if l.path == "" {
		return fmt.Errorf("locations: no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Where this source's data was provisioned, written by `varhub download`.\n")
	b.WriteString("#\n")
	b.WriteString("# The manifest beside this file says what the source is and stays portable;\n")
	b.WriteString("# this says where this machine put it. Delete it to fall back to resolving\n")
	b.WriteString("# under cache_dir.\n\n")
	if err := toml.NewEncoder(&b).Encode(l); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Delete removes the overlay, so the source resolves by convention again.
func (l *Locations) Delete() error {
	if l == nil || l.path == "" {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HasLocations reports whether a recorded location exists for this source.
func (s Source) HasLocations() bool { return !s.locations.Empty() }
