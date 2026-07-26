package registry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

func mkdirWrite(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func TestFetchManifestAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := writeFiles(dir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer ts.Close()
	ctx := context.Background()

	m, err := Fetch(ctx, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if m.Snapshot("2026-06") == nil {
		t.Fatal("snapshot 2026-06 not found in manifest")
	}
	src := m.Source("clinvar", "")
	if src == nil || src.Version != "2026-01" {
		t.Fatalf("source clinvar not found / wrong: %+v", src)
	}

	rel, err := FetchFragment(ctx, ts.URL, m.Snapshot("2026-06").File)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Sources) != 1 || rel.Sources[0].ID() != "clinvar:2026-01" {
		t.Errorf("fetched snapshot sources = %+v", rel.Sources)
	}
	if len(rel.Annotations) != 1 || rel.Annotations[0].Name != "clinvar_sig" {
		t.Errorf("fetched snapshot annotations = %+v", rel.Annotations)
	}
}

func writeFiles(dir string) error {
	if err := mkdirWrite(dir+"/registry.toml", `
[[snapshots]]
name = "2026-06"
file = "snapshots/2026-06.toml"
description = "test"
[[sources]]
name = "clinvar"
version = "2026-01"
file = "sources/clinvar.toml"
`); err != nil {
		return err
	}
	body := `
[[sources]]
name = "clinvar"
version = "2026-01"
format = "vcf"
url = "https://ex.org/c.vcf.gz"
  [[sources.annotations]]
  name = "clinvar_sig"
  field = "CLNSIG"
  type = "categorical"
`
	if err := mkdirWrite(dir+"/snapshots/2026-06.toml", body); err != nil {
		return err
	}
	return mkdirWrite(dir+"/sources/clinvar.toml", body)
}

// A location given as the explicit registry.toml URL resolves the same way.
func TestFetchManifestExplicitURL(t *testing.T) {
	dir := t.TempDir()
	if err := writeFiles(dir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer ts.Close()

	m, err := Fetch(context.Background(), ts.URL+"/registry.toml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Source("clinvar", "2026-01") == nil {
		t.Fatal("source not found via explicit registry.toml URL")
	}
	rel, err := FetchFragment(context.Background(), ts.URL+"/registry.toml", "sources/clinvar.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Sources) != 1 {
		t.Errorf("file should resolve against the manifest dir: %+v", rel.Sources)
	}
}

func TestRenderSnippetRoundTrip(t *testing.T) {
	in := &config.Snapshot{Sources: []config.Source{{
		Name: "gnomad", Version: "4.1", Assembly: "GRCh38", Format: "vcf", URL: "https://x/g.vcf.gz",
		Annotations: []config.Annotation{{Name: "af", Field: "AF", Type: "numeric"}},
	}}}
	out, err := RenderSnippet(in)
	if err != nil {
		t.Fatal(err)
	}
	var back config.Snapshot
	if _, err := toml.Decode(out, &back); err != nil {
		t.Fatalf("decode rendered snippet: %v", err)
	}
	back.Normalize()
	if len(back.Sources) != 1 || back.Sources[0].ID() != "gnomad:4.1" || back.Sources[0].Assembly != "GRCh38" {
		t.Errorf("sources round-trip: %+v", back.Sources)
	}
	if len(back.Annotations) != 1 || back.Annotations[0].Name != "af" {
		t.Errorf("annotations round-trip: %+v", back.Annotations)
	}
}

func TestSubmitIssue(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"html_url":"https://github.com/compgenlab/varianthub-public-data-registry/issues/7"}`)
	}))
	defer ts.Close()

	s := GitHubSubmitter{Token: "tok123", APIBase: ts.URL}
	url, err := s.SubmitIssue(context.Background(), "compgenlab/varianthub-public-data-registry", "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/compgenlab/varianthub-public-data-registry/issues" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth = %q", gotAuth)
	}
	labels, _ := payload["labels"].([]any)
	if len(labels) != 1 || labels[0] != SubmissionLabel {
		t.Errorf("labels = %v, want [%s]", payload["labels"], SubmissionLabel)
	}
	if !strings.HasSuffix(url, "/issues/7") {
		t.Errorf("url = %q", url)
	}
}

// TestManifestSourceLatest: an empty version or ":latest" resolves to the entry
// flagged latest; a sole version resolves without a flag; multiple versions with no
// flag is an error.
func TestManifestSourceLatest(t *testing.T) {
	m := &Manifest{Sources: []Entry{
		{Name: "dbSNP", Version: "b156"},
		{Name: "dbSNP", Version: "b157", Latest: true},
		{Name: "revel", Version: "1.3"},
	}}
	for _, v := range []string{"", "latest"} {
		if e := m.Source("dbSNP", v); e == nil || e.Version != "b157" {
			t.Errorf("Source(dbSNP, %q) = %+v, want b157 (latest)", v, e)
		}
	}
	if e := m.Source("dbSNP", "b156"); e == nil || e.Version != "b156" {
		t.Errorf("Source(dbSNP, b156) = %+v, want exact b156", e)
	}
	if e := m.Source("revel", ""); e == nil || e.Version != "1.3" {
		t.Errorf("Source(revel, \"\") = %+v, want sole 1.3", e)
	}
	ambig := &Manifest{Sources: []Entry{{Name: "x", Version: "1"}, {Name: "x", Version: "2"}}}
	if _, err := ambig.SourceE("x", ""); err == nil {
		t.Error("SourceE(x, \"\") with two unflagged versions should error")
	}
}
