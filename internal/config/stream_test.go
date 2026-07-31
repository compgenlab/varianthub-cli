package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A streamed source resolves to its URL: there is no cached copy to point at.
func TestStreamResolvesToURL(t *testing.T) {
	c := &Config{dir: t.TempDir(), DataDir: "data", CacheDir: "data/cache"}
	src := Source{
		Name: "gnomad", Version: "4.1", Format: "vcf", Stream: true,
		URL: "https://storage.googleapis.com/gnomad/gnomad.v4.1.sites.chr1.vcf.bgz",
	}
	// ResolveSourcePath answers the *provisioning* question — where would a
	// copy go — so it stays a cache path even for a streamed source. Resolving
	// it to the url made `download --no-stream` write to a directory named
	// after the url instead of into the cache.
	if got := c.ResolveSourcePath(src); !strings.Contains(got, "data/cache") {
		t.Errorf("ResolveSourcePath = %q, want a cache target", got)
	}
	files := c.ResolveSourceFiles(src)
	if len(files) != 1 || files[0].Path != src.URL {
		t.Fatalf("ResolveSourceFiles = %+v", files)
	}
	// Local means "never download"; a streamed source is not that — it has no
	// local copy at all, and conflating them would change fetch's behaviour.
	if files[0].Local {
		t.Error("a streamed source should not be marked Local")
	}
}

// A non-streamed source is unaffected.
func TestStreamOffKeepsCacheResolution(t *testing.T) {
	dir := t.TempDir()
	c := &Config{dir: dir, DataDir: "data", CacheDir: "data/cache"}
	src := Source{Name: "clinvar", Version: "1", Format: "vcf", URL: "https://x/clinvar.vcf.gz"}
	got := c.ResolveSourcePath(src)
	want := filepath.Join(dir, "data/cache", "clinvar", "1", "clinvar.vcf.gz")
	if got != want {
		t.Errorf("ResolveSourcePath = %q, want %q", got, want)
	}
}

func TestStreamValidation(t *testing.T) {
	base := Source{Name: "s", Version: "1", Format: "vcf", Stream: true}
	for _, tc := range []struct {
		name string
		mod  func(*Source)
		want string
	}{
		{"no url", func(s *Source) { s.URL = "" }, "url"},
		{"with localpath", func(s *Source) { s.URL = "https://x/a.gz"; s.LocalPath = "/tmp/a.gz" }, "localpath"},
		{"with build", func(s *Source) { s.URL = "https://x/a.gz"; s.Build = &SourceBuild{} }, "build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mod(&s)
			snap := &Snapshot{Sources: []Source{s}}
			err := snap.validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q: %v", tc.want, err)
			}
		})
	}
	// The valid shape passes.
	ok := base
	ok.URL = "https://x/a.vcf.gz"
	if err := (&Snapshot{Sources: []Source{ok}}).validate(); err != nil {
		t.Errorf("a valid streamed source was rejected: %v", err)
	}
}

// The overlay overrides where a source is read from, without making it Local.
func TestOverlayOverridesReadPath(t *testing.T) {
	dir := t.TempDir()
	c := &Config{dir: dir, DataDir: "data", CacheDir: "data/cache", AnnotationsDir: "annotations"}
	src := Source{Name: "gnomad", Version: "4.1", Format: "vcf",
		URL: "https://x/gnomad.vcf.gz", Chroms: []string{"chr1", "chr2"}}
	src.URL = "https://x/gnomad.{chrom}.vcf.gz"

	// Convention first.
	targets := c.ResolveSourceTargets(src)
	if len(targets) != 2 || !strings.Contains(targets[0].Path, "data/cache") {
		t.Fatalf("targets = %+v", targets)
	}

	// Now record chr1 in a bucket, leaving chr2 alone: the mixed case.
	src.locations = &Locations{Files: []FileLocation{
		{Chrom: "chr1", Path: "s3://bucket/gnomad/4.1/gnomad.chr1.vcf.gz",
			Index: "s3://bucket/gnomad/4.1/gnomad.chr1.vcf.gz.tbi"},
	}}
	files := c.ResolveSourceFiles(src)
	if files[0].Path != "s3://bucket/gnomad/4.1/gnomad.chr1.vcf.gz" {
		t.Errorf("chr1 = %q, want the recorded bucket location", files[0].Path)
	}
	if files[0].IndexPath != "s3://bucket/gnomad/4.1/gnomad.chr1.vcf.gz.tbi" {
		t.Errorf("chr1 index = %q", files[0].IndexPath)
	}
	if files[0].Local {
		t.Error("an overlay entry must not mark the file Local")
	}
	// chr2 has no entry and keeps resolving by convention.
	if !strings.Contains(files[1].Path, "data/cache") {
		t.Errorf("chr2 = %q, want the cache_dir convention", files[1].Path)
	}
	// Provisioning targets are unaffected by the overlay.
	if strings.HasPrefix(c.ResolveSourceTargets(src)[0].Path, "s3://") {
		t.Error("the overlay leaked into the provisioning target")
	}
}

// The bug this pins: with `--no-stream` the download target must be the cache,
// not a directory named after the url.
func TestStreamProvisioningTargetIsTheCache(t *testing.T) {
	dir := t.TempDir()
	c := &Config{DataDir: "data", CacheDir: "data/cache"}
	c.SetBaseDir(dir)
	src := Source{
		Name: "s", Version: "1", Format: "vcf", Stream: true,
		URL: "http://example.org/s.vcf.gz",
	}
	for _, f := range c.ResolveSourceTargets(src) {
		if strings.Contains(f.Path, "://") || strings.HasPrefix(f.Path, "http") {
			t.Errorf("provisioning target is a url, not a path: %q", f.Path)
		}
		if !strings.HasPrefix(f.Path, dir) {
			t.Errorf("target %q is outside the config base %q", f.Path, dir)
		}
	}
}
