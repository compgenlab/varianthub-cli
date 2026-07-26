package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cganno/internal/config"
)

// TestSourceReproducible: a registry source must be reconstructable without local
// files — via a URL, a build recipe, or a multi-file union where every file has a URL.
func TestSourceReproducible(t *testing.T) {
	cases := []struct {
		name string
		src  config.Source
		want bool
	}{
		{"url", config.Source{URL: "https://x/a.vcf.gz"}, true},
		{"build", config.Source{Build: &config.SourceBuild{Output: "o.gz"}}, true},
		{"localpath-only", config.Source{LocalPath: "/data/a.vcf.gz"}, false},
		{"empty", config.Source{}, false},
		{"multi-file all urls", config.Source{Files: []config.FileSpec{
			{URL: "https://x/a.gz", LocalPath: "/d/a.gz"}, {URL: "https://x/b.gz"},
		}}, true},
		{"multi-file missing a url", config.Source{Files: []config.FileSpec{
			{URL: "https://x/a.gz"}, {LocalPath: "/d/b.gz"},
		}}, false},
		{"tool docker ref", config.Source{Type: "tool", Image: "docker://ensemblorg/ensembl-vep"}, true},
		{"tool sif url", config.Source{Type: "tool", Image: "https://x/vep.sif"}, true},
		{"tool local sif", config.Source{Type: "tool", Image: "/opt/vep.sif"}, false},
		{"tool no image", config.Source{Type: "tool"}, false},
	}
	for _, c := range cases {
		if got := sourceReproducible(c.src); got != c.want {
			t.Errorf("%s: sourceReproducible = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBundleAssets: bundleAssets produces a base64 gzip'd tar that round-trips back to
// the original files (flat, by basename) — the transport the issue workflow unpacks.
func TestBundleAssets(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{
		"expand_vep_vcf.py":            "#!/usr/bin/env python3\nprint('expand')\n",
		"vep_vcf_worst_consequence.py": "#!/usr/bin/env python3\nprint('worst')\n",
	}
	for n, c := range want {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	b64, err := bundleAssets(dir, []string{"expand_vep_vcf.py", "vep_vcf_worst_consequence.py"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(b64, "\n", ""))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[h.Name] = string(data)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("member %q = %q, want %q", n, got[n], c)
		}
	}
}

// TestLocalAssets: URLs and absolute paths are dropped (they travel as-is in the
// toml); relative co-located names are kept for bundling.
func TestLocalAssets(t *testing.T) {
	got := localAssets([]string{"helper.py", "https://x/y.py", "/abs/z.py", "sub/a.py"})
	if strings.Join(got, ",") != "helper.py,sub/a.py" {
		t.Errorf("localAssets = %v, want [helper.py sub/a.py]", got)
	}
}
