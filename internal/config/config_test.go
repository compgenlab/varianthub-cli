package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSelectAnnotations(t *testing.T) {
	snap := &Snapshot{Annotations: []Annotation{
		{Name: "a", Default: true}, {Name: "b"}, {Name: "c", Default: true},
	}}
	if got, _ := snap.SelectAnnotations(nil, true); len(got) != 3 {
		t.Errorf("all = %d, want 3", len(got))
	}
	def, _ := snap.SelectAnnotations(nil, false)
	if len(def) != 2 || def[0].Name != "a" || def[1].Name != "c" {
		t.Errorf("default set = %+v, want [a c]", def)
	}
	ex, err := snap.SelectAnnotations([]string{"b"}, false)
	if err != nil || len(ex) != 1 || ex[0].Name != "b" {
		t.Errorf("explicit = %+v err=%v", ex, err)
	}
	if _, err := snap.SelectAnnotations([]string{"zzz"}, false); err == nil {
		t.Error("expected an unknown-key error")
	}
}

func TestReflowLongArrays(t *testing.T) {
	in := strings.Join([]string{
		`requires = ["unzip", "python3", "bgzip"]`,
		`chroms = ["1", "2", "X", "Y"]`,
		`  inputs = ["https://example.com/revel-v1.3_chrom_21_042749014_048084282.csv.zip", "https://example.com/revel-v1.3_chrom_22.csv.zip"]`,
		`empty = []`,
	}, "\n")
	got := reflowLongArrays(in)
	want := strings.Join([]string{
		`requires = ["unzip", "python3", "bgzip"]`, // short elements: stays inline
		`chroms = ["1", "2", "X", "Y"]`,            // short elements: stays inline
		`  inputs = [`,                             // long URL elements: wrapped, indent preserved
		`    "https://example.com/revel-v1.3_chrom_21_042749014_048084282.csv.zip",`,
		`    "https://example.com/revel-v1.3_chrom_22.csv.zip",`,
		`  ]`,
		`empty = []`, // empty: untouched
	}, "\n")
	if got != want {
		t.Errorf("reflowLongArrays mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestReflowLongArraysRoundTrips(t *testing.T) {
	snap := &Snapshot{Sources: []Source{{
		Name: "revel", Version: "1.3", Format: "tab",
		Build: &SourceBuild{
			Output: "out.txt.gz",
			Inputs: []string{
				"https://example.com/revel-v1.3_chrom_21_042749014_048084282.csv.zip",
				"https://example.com/revel-v1.3_chrom_22_016157310_025437413.csv.zip",
			},
			Run: []string{"unzip {inputs}/*.zip", "python3 conv.py | bgzip > {output}"},
		},
	}}}
	out, err := MarshalSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "inputs = [\n") {
		t.Errorf("expected inputs reflowed to multi-line, got:\n%s", out)
	}
	// The reflowed TOML must still decode to the same inputs.
	var got Snapshot
	if _, err := toml.Decode(out, &got); err != nil {
		t.Fatalf("reflowed TOML does not parse: %v\n%s", err, out)
	}
	if len(got.Sources) != 1 || len(got.Sources[0].Build.Inputs) != 2 {
		t.Errorf("round-trip lost data: %+v", got.Sources)
	}
}

func TestIDUsesColon(t *testing.T) {
	if got := (Source{Name: "revel", Version: "1.3"}).ID(); got != "revel:1.3" {
		t.Errorf("Source.ID() = %q, want revel:1.3", got)
	}
	if got := (Tool{Name: "vep", Version: "113"}).ID(); got != "vep:113" {
		t.Errorf("Tool.ID() = %q, want vep:113", got)
	}
}

// TestToolDataDirUsesNameVersion: the on-disk tool data dir is name/version (like
// the image cache), never the ":" ID — a ":" in a directory path is unsafe.
func TestToolDataDirUsesNameVersion(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	got := cfg.ResolveToolData(Tool{Name: "vep", Version: "113"})
	if !strings.HasSuffix(got, filepath.Join("tools", "vep", "113")) {
		t.Errorf("ResolveToolData = %q, want suffix tools/vep/113", got)
	}
	if strings.ContainsRune(got, ':') {
		t.Errorf("tool data dir must not contain ':' — %q", got)
	}
}

func TestToolRequiredSoftware(t *testing.T) {
	// Container tool: engine is appended automatically and deduped.
	ct := Tool{Requires: []string{"python3", "bgzip", "apptainer"}, Steps: []Step{{Container: true}}}
	got := ct.RequiredSoftware()
	want := []string{"python3", "bgzip", "apptainer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("container tool = %v, want %v", got, want)
	}

	// Container tool that doesn't list the engine: it's added.
	it := Tool{Requires: []string{"python3"}, Image: "docker://x"}
	if got := it.RequiredSoftware(); strings.Join(got, ",") != "python3,apptainer" {
		t.Errorf("image tool = %v, want [python3 apptainer]", got)
	}

	// Non-container tool: no engine appended.
	nt := Tool{Requires: []string{"python3"}, Steps: []Step{{Container: false}}}
	if got := nt.RequiredSoftware(); strings.Join(got, ",") != "python3" {
		t.Errorf("non-container tool = %v, want [python3]", got)
	}
}

func TestDropSource(t *testing.T) {
	snap := &Snapshot{Sources: []Source{
		{Name: "clinvar", Version: "1", Annotations: []Annotation{{Name: "sig"}, {Name: "sig2"}}},
		{Name: "gnomad", Version: "4", Annotations: []Annotation{{Name: "af"}}},
	}}
	snap.Normalize()
	if removed := snap.DropSource("clinvar"); removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if len(snap.Sources) != 1 || snap.Sources[0].Name != "gnomad" {
		t.Errorf("sources = %+v", snap.Sources)
	}
	if len(snap.Annotations) != 1 || snap.Annotations[0].Name != "af" {
		t.Errorf("annotations = %+v, want only af", snap.Annotations)
	}
}

func TestNormalizeSetsSource(t *testing.T) {
	snap := &Snapshot{
		Sources: []Source{
			{Name: "clinvar", Version: "1", Annotations: []Annotation{{Name: "sig"}}},
			{Type: "builtin", Annotations: []Annotation{{Builtin: "tstv"}}},
			{Type: "tool", Name: "vep", Version: "1", Steps: []Step{{Run: "x"}},
				Annotations: []Annotation{{Name: "csq"}}},
		},
	}
	snap.Normalize()
	got := map[string]string{}
	for _, a := range snap.Annotations {
		got[a.Source] = a.Name // key empty for builtin
	}
	if _, ok := got["clinvar"]; !ok {
		t.Errorf("clinvar annotation missing source: %+v", snap.Annotations)
	}
	if _, ok := got["tstv"]; !ok {
		t.Errorf("builtin source not set to tstv: %+v", snap.Annotations)
	}
	if _, ok := got["vep"]; !ok {
		t.Errorf("tool annotation missing source vep: %+v", snap.Annotations)
	}
}

func TestNormalizeDefaultsBuiltinName(t *testing.T) {
	snap := &Snapshot{
		Sources: []Source{
			{Type: "builtin", Annotations: []Annotation{
				{Builtin: "auto_id"},             // no name → defaults to the builtin name
				{Builtin: "tstv", Name: "ts_tv"}, // explicit name preserved
			}},
		},
	}
	snap.Normalize()

	// The source's own annotations are defaulted in place (the overlay BuiltinSource
	// reads these and keys output rows on Name).
	if got := snap.Sources[0].Annotations[0].Name; got != "auto_id" {
		t.Errorf("builtin without name: Name = %q, want auto_id", got)
	}
	if got := snap.Sources[0].Annotations[1].Name; got != "ts_tv" {
		t.Errorf("builtin with explicit name overwritten: Name = %q, want ts_tv", got)
	}
	// The flat list carries the resolved names too.
	names := map[string]bool{}
	for _, a := range snap.Annotations {
		names[a.Name] = true
	}
	if !names["auto_id"] || !names["ts_tv"] {
		t.Errorf("flat annotations missing resolved names: %+v", snap.Annotations)
	}
}

func TestDisplayTitleFallsBackToSlug(t *testing.T) {
	if got := (Source{Name: "clinvar"}).DisplayTitle(); got != "clinvar" {
		t.Errorf("source with no title: DisplayTitle = %q, want clinvar", got)
	}
	if got := (Source{Name: "clinvar", Title: "ClinVar"}).DisplayTitle(); got != "ClinVar" {
		t.Errorf("source title: DisplayTitle = %q, want ClinVar", got)
	}
	if got := (&Snapshot{Name: "2026-07"}).DisplayTitle(); got != "2026-07" {
		t.Errorf("snapshot with no title: DisplayTitle = %q, want 2026-07", got)
	}
	if got := (&Snapshot{Name: "2026-07", Title: "July Panel"}).DisplayTitle(); got != "July Panel" {
		t.Errorf("snapshot title: DisplayTitle = %q, want July Panel", got)
	}
}

func TestSnapshotManifestTitleRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.toml")
	in := &SnapshotConfig{Title: "July 2026 Clinical Panel", Description: "d", Sources: []string{"builtins:1"}}
	if err := WriteSnapshotConfig(path, in); err != nil {
		t.Fatal(err)
	}
	sc, err := ReadSnapshotConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Title != "July 2026 Clinical Panel" {
		t.Errorf("title = %q, want round-tripped value", sc.Title)
	}
}

func TestGeneListResolveAndValidate(t *testing.T) {
	snap := &Snapshot{
		Sources: []Source{
			{Name: "gencode", Version: "48", Format: "gtf", URL: "https://x/gencode.v48.gtf.gz"},
			{Type: "genelist", Name: "germline_cancer_genes", Version: "1", GTF: "gencode:48",
				Genes:       []string{"BRCA1", "BRCA2"},
				Annotations: []Annotation{{Name: "germline_cancer_gene"}}}, // no type → defaults to flag
		},
	}
	if err := snap.resolveGeneLists(); err != nil {
		t.Fatalf("resolveGeneLists: %v", err)
	}
	gl := &snap.Sources[1]
	if gl.GTFRef == nil || gl.GTFRef.Name != "gencode" {
		t.Fatalf("gtf ref not resolved: %+v", gl.GTFRef)
	}
	if gl.Annotations[0].Type != "flag" {
		t.Errorf("genelist annotation type = %q, want flag (defaulted)", gl.Annotations[0].Type)
	}
	snap.normalize()
	if err := snap.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The flat annotation is attributed to the genelist source.
	found := false
	for _, a := range snap.Annotations {
		if a.Name == "germline_cancer_gene" {
			found = true
			if a.Source != "germline_cancer_genes" {
				t.Errorf("annotation source = %q, want germline_cancer_genes", a.Source)
			}
		}
	}
	if !found {
		t.Error("germline_cancer_gene annotation missing from flat list")
	}
}

func TestGeneListUnknownGTFRejected(t *testing.T) {
	snap := &Snapshot{Sources: []Source{
		{Type: "genelist", Name: "gl", Version: "1", GTF: "nope:1",
			Genes: []string{"BRCA1"}, Annotations: []Annotation{{Name: "x"}}},
	}}
	if err := snap.resolveGeneLists(); err == nil {
		t.Error("expected an error when the referenced GTF source is absent")
	}
}

func TestGeneSetInlineAndFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "data_dir = \"data\"\nannotations_dir = \"ann\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// genes_file lives alongside the source fragment: ann/sources/gl/1/genes.txt
	fragDir := filepath.Dir(cfg.SourceFile("gl", "1"))
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fragDir, "genes.txt"),
		[]byte("# cancer genes\nMLH1\nMSH2\n\nPMS2\t extra columns ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := Source{Type: "genelist", Name: "gl", Version: "1",
		Genes: []string{"BRCA1", "brca2"}, GenesFile: "genes.txt"}
	set, err := cfg.GeneSet(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BRCA1", "BRCA2", "MLH1", "MSH2", "PMS2"} {
		if !set[want] {
			t.Errorf("gene set missing %q (upper-cased); got %v", want, set)
		}
	}
	if len(set) != 5 {
		t.Errorf("gene set size = %d, want 5", len(set))
	}
}

func TestResolveSourceFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndata_dir = \"data\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	single := cfg.ResolveSourceFiles(Source{Name: "clinvar", Version: "1", URL: "https://x/clinvar.vcf.gz"})
	if len(single) != 1 || single[0].Chrom != "" || single[0].Local {
		t.Errorf("single-file = %+v", single)
	}
	// localpath ⇒ Local file, used exactly.
	loc := cfg.ResolveSourceFiles(Source{Name: "g", Version: "1", URL: "https://x/g.vcf.gz", LocalPath: "/data/g.vcf.gz"})
	if len(loc) != 1 || !loc[0].Local || loc[0].Path != "/data/g.vcf.gz" {
		t.Errorf("localpath = %+v", loc)
	}
	multi := cfg.ResolveSourceFiles(Source{
		Name: "gnomad", Version: "4", URL: "https://x/g.{chrom}.vcf.gz",
		Checksum: "md5:https://x/g.{chrom}.md5", Chroms: []string{"chr1", "chr2"},
	})
	if len(multi) != 2 || multi[0].Chrom != "chr1" {
		t.Fatalf("multi-file = %+v", multi)
	}
	if !strings.Contains(multi[0].URL, "g.chr1.vcf.gz") || !strings.Contains(multi[0].Checksum, "g.chr1.md5") {
		t.Errorf("chr1 expansion = url:%q checksum:%q", multi[0].URL, multi[0].Checksum)
	}
	union := cfg.ResolveSourceFiles(Source{Name: "dbnsfp", Version: "4", Files: []FileSpec{
		{URL: "https://x/coding.tsv.gz", Checksum: "md5:abc"},
		{URL: "https://x/indels.tsv.gz", Checksum: "md5:def"},
	}})
	if len(union) != 2 || !strings.Contains(union[0].Path, "coding.tsv.gz") || union[1].Checksum != "md5:def" {
		t.Errorf("files union = %+v", union)
	}
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	// These tests place fragments directly under dir/sources|tools|snapshots (see
	// writeSnapshot), so pin annotations_dir to "." unless the body overrides it —
	// the production default is now "annotations".
	if !strings.Contains(body, "annotations_dir") {
		body = "annotations_dir = \".\"\n" + body
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSnapshot writes a source/tool fragment body into the v2 top-level tree
// (sources/<name>/<version>/… or tools/…) and appends a name:version ref to the
// snapshots/<snap>.toml manifest. `frag` (the legacy filename) is ignored.
func writeSnapshot(t *testing.T, dir, snap, frag, body string) {
	t.Helper()
	_ = frag
	var f Snapshot
	if _, err := toml.Decode(body, &f); err != nil {
		t.Fatalf("decode fragment: %v", err)
	}
	if len(f.Sources) == 0 {
		t.Fatalf("fragment has no source: %s", body)
	}
	s := f.Sources[0] // data, builtin, or tool source
	name, ver := s.Name, s.Version
	if s.IsBuiltinSource() && name == "" {
		name, ver = "builtins", "1"
	}
	file := filepath.Join(dir, "sources", name, ver, name+"-"+ver+".toml")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Append the ref to the manifest (create it if absent).
	mpath := filepath.Join(dir, "snapshots", snap+".toml")
	sc := &SnapshotConfig{}
	if b, err := os.ReadFile(mpath); err == nil {
		toml.Decode(string(b), sc)
	}
	ref := name + ":" + ver
	has := false
	for _, r := range sc.Sources {
		if r == ref {
			has = true
		}
	}
	if !has {
		sc.Sources = append(sc.Sources, ref)
	}
	if err := os.MkdirAll(filepath.Dir(mpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshotConfig(mpath, sc); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, `
assembly = "GRCh38"
data_dir = "data"
default_snapshot = "2026-06"
[database]
backend = "sqlite"
`)
	writeSnapshot(t, dir, "2026-06", "01_clinvar.toml", `
[[sources]]
name = "clinvar"
version = "2026-01"
format = "vcf"
localpath = "clinvar/clinvar.vcf.gz"
  [[sources.annotations]]
  name = "clinvar_sig"
  field = "CLNSIG"
  type = "categorical"
  [[sources.annotations]]
  name = "af"
  type = "numeric"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Backend != "sqlite" || cfg.Database.Path != "varhub.db" {
		t.Errorf("defaults wrong: %+v", cfg.Database)
	}

	snap, err := cfg.LoadSnapshot("") // default
	if err != nil {
		t.Fatal(err)
	}
	if snap.Name != "2026-06" || len(snap.Sources) != 1 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if snap.Sources[0].ID() != "clinvar:2026-01" {
		t.Errorf("source id = %q", snap.Sources[0].ID())
	}
	if len(snap.Annotations) != 2 || snap.Annotations[0].Source != "clinvar" {
		t.Fatalf("flat annotations = %+v", snap.Annotations)
	}
	if snap.Annotations[1].FieldName() != "af" || !snap.Annotations[1].IsNumeric() {
		t.Errorf("af annotation = %+v", snap.Annotations[1])
	}
	want := filepath.Join(dir, "data", "clinvar/clinvar.vcf.gz")
	if got := cfg.ResolveSourcePath(snap.Sources[0]); got != want {
		t.Errorf("ResolveSourcePath = %q, want %q", got, want)
	}
}

func TestResolveSourcePathCacheVsLocal(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndata_dir = \"data\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cached := cfg.ResolveSourcePath(Source{Name: "clinvar", Version: "2026-01", URL: "https://ex.org/clinvar.vcf.gz"})
	want := filepath.Join(dir, "data", "cache", "clinvar", "2026-01", "clinvar.vcf.gz")
	if cached != want {
		t.Errorf("cache path = %q, want %q", cached, want)
	}
	expl := cfg.ResolveSourcePath(Source{Name: "g", Version: "1", LocalPath: "g/g.bed.gz"})
	if expl != filepath.Join(dir, "data", "g/g.bed.gz") {
		t.Errorf("localpath = %q", expl)
	}
	if got := cfg.ResolveSourcePath(Source{LocalPath: "/abs/x.gz"}); got != "/abs/x.gz" {
		t.Errorf("absolute localpath = %q, want /abs/x.gz", got)
	}
}

func TestLocalPathEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndata_dir = \"data\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYDATA", "/srv/refs")
	// Absolute after expansion → used as-is.
	if got := cfg.ResolveSourcePath(Source{LocalPath: "${MYDATA}/clinvar.vcf.gz"}); got != "/srv/refs/clinvar.vcf.gz" {
		t.Errorf("env localpath = %q, want /srv/refs/clinvar.vcf.gz", got)
	}
	// $VAR form + index path.
	if got := cfg.resolveLocalIndex(Source{LocalPathIndex: "$MYDATA/clinvar.vcf.gz.tbi"}); got != "/srv/refs/clinvar.vcf.gz.tbi" {
		t.Errorf("env localpath_index = %q", got)
	}
	// Relative after expansion → joined under data_dir.
	t.Setenv("SUB", "sub")
	if got := cfg.ResolveSourcePath(Source{LocalPath: "${SUB}/x.gz"}); got != filepath.Join(dir, "data", "sub/x.gz") {
		t.Errorf("relative env localpath = %q", got)
	}
}

func TestLoadInterpolatesVarhubHome(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
assembly = "GRCh38"
data_dir = "$VARHUB_HOME/data"
[database]
backend = "sqlite"
path = "${VARHUB_HOME}/varhub.db"
[references.GRCh38]
fasta = "$KEEP_ME/ref.fa"
`)

	t.Setenv("VARHUB_HOME", "/home/x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/home/x/data" {
		t.Errorf("data_dir = %q, want /home/x/data", cfg.DataDir)
	}
	if cfg.Database.Path != "/home/x/varhub.db" {
		t.Errorf("database.path = %q, want /home/x/varhub.db", cfg.Database.Path)
	}
	if got := cfg.ReferenceFor("GRCh38"); got != "${KEEP_ME}/ref.fa" {
		t.Errorf("fasta = %q, want ${KEEP_ME}/ref.fa", got)
	}
}

func TestLoadVarhubHomeDefaultsToDot(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndata_dir = \"$VARHUB_HOME/data\"\n")
	t.Setenv("VARHUB_HOME", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "./data" {
		t.Errorf("data_dir = %q, want ./data", cfg.DataDir)
	}
}

func TestDatabasePathAbs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")

	rel := writeConfig(t, dir, "assembly = \"GRCh38\"\n[database]\nbackend = \"sqlite\"\npath = \"varhub.db\"\n")
	cfg, err := Load(rel)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.DatabasePathAbs(), filepath.Join(dir, "varhub.db"); got != want {
		t.Errorf("relative DatabasePathAbs = %q, want %q", got, want)
	}

	abs := writeConfig(t, dir, "assembly = \"GRCh38\"\n[database]\nbackend = \"sqlite\"\npath = \"/var/lib/v.db\"\n")
	cfg, err = Load(abs)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DatabasePathAbs(); got != "/var/lib/v.db" {
		t.Errorf("absolute DatabasePathAbs = %q, want /var/lib/v.db", got)
	}

	cfg.Database.Backend = "postgres"
	cfg.Database.Path = "postgres://u@h/db"
	if got := cfg.DatabasePathAbs(); got != "postgres://u@h/db" {
		t.Errorf("postgres DatabasePathAbs = %q, want the DSN verbatim", got)
	}
}

// TestSnapshotReferenceFromAssembly: a snapshot's reference FASTA is looked up from
// the config's per-assembly [references.<assembly>] map, keyed by the snapshot's
// assembly — not pinned in the manifest.
func TestSnapshotReferenceFromAssembly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, `
default_snapshot = "r"
[references.GRCh38]
fasta = "/ref/GRCh38.fa"
[references.GRCh37]
fasta = "/ref/GRCh37.fa"
`)
	if err := WriteSnapshotConfig(filepath.Join(dir, "snapshots", "r.toml"), &SnapshotConfig{Assembly: "GRCh37"}); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, dir, "r", "x.toml", `
[[sources]]
name = "x"
version = "1"
assembly = "GRCh37"
format = "vcf"
localpath = "x.vcf.gz"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("r")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reference != "/ref/GRCh37.fa" {
		t.Errorf("snapshot reference = %q, want /ref/GRCh37.fa (from its GRCh37 assembly)", snap.Reference)
	}
}

func TestSourceAssemblyVerification(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "default_snapshot = \"r\"\n[database]\nbackend = \"sqlite\"\n")

	// Assembly is per-snapshot now: the manifest carries it (writeSnapshot preserves it).
	if err := WriteSnapshotConfig(filepath.Join(dir, "snapshots", "r.toml"), &SnapshotConfig{Assembly: "GRCh38"}); err != nil {
		t.Fatal(err)
	}

	writeSnapshot(t, dir, "r", "01_clinvar.toml", `
[[sources]]
name = "clinvar"
version = "1"
assembly = "GRCh37"
format = "vcf"
localpath = "c.vcf.gz"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LoadSnapshot("r"); err == nil {
		t.Fatal("expected an assembly-mismatch error")
	}

	writeSnapshot(t, dir, "r", "01_clinvar.toml", `
[[sources]]
name = "clinvar"
version = "1"
assembly = "GRCh38"
format = "vcf"
localpath = "c.vcf.gz"
`)
	if _, err := cfg.LoadSnapshot("r"); err != nil {
		t.Fatalf("matching assembly should load: %v", err)
	}
}

func TestSourceChecksumSpecValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndefault_snapshot = \"r\"\n[database]\nbackend = \"sqlite\"\n")
	writeSnapshot(t, dir, "r", "01_x.toml", `
[[sources]]
name = "x"
version = "1"
format = "vcf"
localpath = "x.vcf.gz"
checksum = "sha256:nothex"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LoadSnapshot("r"); err == nil {
		t.Fatal("expected a checksum-spec validation error")
	}
}

func TestRegistryLocations(t *testing.T) {
	c := &Config{}
	if got := c.RegistryLocations(); len(got) != 1 || got[0] != DefaultRegistry {
		t.Errorf("default = %v, want [%s]", got, DefaultRegistry)
	}
	c = &Config{RegistryURL: "https://x/registry.toml"}
	if got := c.RegistryLocations(); len(got) != 1 || got[0] != "https://x/registry.toml" {
		t.Errorf("registry_url = %v", got)
	}
	c = &Config{RegistryURL: "https://x/registry.toml", Registries: []string{"https://a/r.toml", "https://b/r.toml"}}
	if got := c.RegistryLocations(); len(got) != 2 || got[0] != "https://a/r.toml" {
		t.Errorf("registries = %v", got)
	}
}

func TestBuiltinSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndefault_snapshot = \"r\"\n[database]\nbackend = \"sqlite\"\n")

	// A builtin source with valid builtins (incl. a parameterized one) loads.
	writeSnapshot(t, dir, "r", "01_builtins.toml", `
[[sources]]
type = "builtin"
  [[sources.annotations]]
  builtin = "tstv"
  [[sources.annotations]]
  builtin = "auto_id"
  [[sources.annotations]]
  builtin = "tags"
  args = "PANEL:v1"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LoadSnapshot("r"); err != nil {
		t.Fatalf("builtin source should load: %v", err)
	}

	// A parameterized builtin without args errors.
	writeSnapshot(t, dir, "r", "01_builtins.toml", "[[sources]]\ntype = \"builtin\"\n  [[sources.annotations]]\n  builtin = \"tags\"\n")
	if _, err := cfg.LoadSnapshot("r"); err == nil {
		t.Fatal("tags without args should error")
	}

	// An unknown builtin errors.
	writeSnapshot(t, dir, "r", "01_builtins.toml", "[[sources]]\ntype = \"builtin\"\n  [[sources.annotations]]\n  builtin = \"nope\"\n")
	if _, err := cfg.LoadSnapshot("r"); err == nil {
		t.Fatal("unknown builtin should error")
	}
}

func TestGTFSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndefault_snapshot = \"r\"\n[database]\nbackend = \"sqlite\"\n")

	// A gtf source selecting valid fields (case-insensitive) + gtf_tags loads.
	writeSnapshot(t, dir, "r", "01_gtf.toml", `
[[sources]]
name = "gencode"
version = "38"
format = "gtf"
url = "https://example/gencode.v38.gtf.gz"
gtf_tags = ["basic"]
  [[sources.annotations]]
  name = "gene"
  field = "GENE"
  [[sources.annotations]]
  name = "region"
  field = "region"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.LoadSnapshot("r"); err != nil {
		t.Fatalf("gtf source should load: %v", err)
	}

	// An unrecognized GTF field errors.
	writeSnapshot(t, dir, "r", "01_gtf.toml", `
[[sources]]
name = "gencode"
version = "38"
format = "gtf"
url = "https://example/gencode.v38.gtf.gz"
  [[sources.annotations]]
  name = "x"
  field = "NOPE"
`)
	if _, err := cfg.LoadSnapshot("r"); err == nil {
		t.Fatal("unrecognized GTF field should error")
	}
}

func TestGTFSourceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01_gencode.toml")
	in := &Snapshot{Sources: []Source{{
		Name: "gencode", Version: "38", Format: "gtf", URL: "https://x/g.gtf.gz",
		GTFTags:     []string{"basic"},
		Annotations: []Annotation{{Name: "gene", Field: "GENE"}, {Name: "region", Field: "REGION"}},
	}}}
	if err := WriteTOML(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFragment(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].Format != "gtf" {
		t.Fatalf("round-trip sources = %+v", out.Sources)
	}
	if got := out.Sources[0].GTFTags; len(got) != 1 || got[0] != "basic" {
		t.Errorf("round-trip gtf_tags = %v, want [basic]", got)
	}
	if len(out.Annotations) != 2 || out.Annotations[0].Source != "gencode" {
		t.Errorf("round-trip annotations = %+v", out.Annotations)
	}
}

// TestConfigOptionalAssemblyAndCache: assembly is snapshot-scoped now (not required
// globally), and the cache is off unless [database] is set.
func TestConfigOptionalAssemblyAndCache(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "data_dir = \"d\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config without assembly/database should load: %v", err)
	}
	if cfg.CacheEnabled() {
		t.Error("cache should be disabled when [database] is absent")
	}
	cfg2, err := Load(writeConfig(t, t.TempDir(), "data_dir=\"d\"\n[database]\nbackend=\"sqlite\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.CacheEnabled() || cfg2.Database.Path != "varhub.db" {
		t.Errorf("cache should be enabled with default path: %+v", cfg2.Database)
	}
}

func TestFragmentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01_gnomad.toml")
	in := &Snapshot{Sources: []Source{{
		Name: "gnomad", Version: "4.1", Format: "vcf", URL: "https://x/g.vcf.gz",
		Annotations: []Annotation{{Name: "af", Type: "numeric"}},
	}}}
	if err := WriteTOML(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFragment(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].ID() != "gnomad:4.1" {
		t.Errorf("round-trip sources = %+v", out.Sources)
	}
	if len(out.Annotations) != 1 || out.Annotations[0].Source != "gnomad" || out.Annotations[0].Name != "af" {
		t.Errorf("round-trip annotations = %+v", out.Annotations)
	}
}

func TestLoadSnapshotParseErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndefault_snapshot = \"r\"\n[database]\nbackend = \"sqlite\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// place a raw source body (unparsed) + a manifest referencing it as x:1.
	place := func(body string) {
		if err := os.MkdirAll(cfg.SourceDir("x", "1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.SourceFile("x", "1"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cfg.SnapshotsPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.SnapshotFile("r"), []byte("sources=[\"x:1\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// TOML syntax error → message reports a line position.
	place("[[sources]]\nname = \"x\nversion = \"1\"\n")
	if _, err := cfg.LoadSnapshot("r"); err == nil || !strings.Contains(err.Error(), "line") {
		t.Errorf("syntax error should report a line position, got: %v", err)
	}

	// Unknown/typo'd key → message names the offending key.
	place("[[sources]]\nname = \"x\"\nversion = \"1\"\nformat = \"vcf\"\nurl = \"https://x\"\ncheksum = \"md5:abc\"\n")
	if _, err := cfg.LoadSnapshot("r"); err == nil || !strings.Contains(err.Error(), "cheksum") {
		t.Errorf("unknown key should be named in the error, got: %v", err)
	}
}

func TestSourceBuild(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", "")
	path := writeConfig(t, dir, "assembly = \"GRCh38\"\ndata_dir = \"data\"\ndefault_snapshot = \"s\"\n[database]\nbackend = \"sqlite\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Build output resolves to a cache path keyed by name/version.
	src := Source{Name: "revel", Version: "1.3", Format: "tab",
		Build: &SourceBuild{Output: "merged.txt.gz", Run: []string{"true"}}}
	want := filepath.Join(dir, "data", "cache", "revel", "1.3", "merged.txt.gz")
	if got := cfg.ResolveSourcePath(src); got != want {
		t.Errorf("build path = %q, want %q", got, want)
	}
	if files := cfg.ResolveSourceFiles(src); len(files) != 1 || files[0].Path != want {
		t.Errorf("ResolveSourceFiles(build) = %+v", files)
	}

	// A build source with a url (provenance) loads; build+localpath conflicts.
	writeSnapshot(t, dir, "s", "01.toml",
		"[[sources]]\nname=\"revel\"\nversion=\"1.3\"\nformat=\"tab\"\nurl=\"https://x\"\n  [sources.build]\n  run=[\"x\"]\n  [[sources.annotations]]\n  name=\"revel\"\n  field=\"7\"\n")
	if _, err := cfg.LoadSnapshot("s"); err != nil {
		t.Fatalf("build+url should load: %v", err)
	}
	writeSnapshot(t, dir, "s", "01.toml",
		"[[sources]]\nname=\"revel\"\nversion=\"1.3\"\nformat=\"tab\"\nlocalpath=\"/x\"\n  [sources.build]\n  run=[\"x\"]\n  [[sources.annotations]]\n  name=\"revel\"\n  field=\"7\"\n")
	if _, err := cfg.LoadSnapshot("s"); err == nil {
		t.Fatal("build + localpath should conflict")
	}
}

// TestPerAltResolveSourceFiles: an {alt} bigwig source expands to one file per
// alt base, each SourceFile carrying its Alt.
func TestPerAltResolveSourceFiles(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	src := Source{
		Name: "am", Version: "1", Format: "bigwig",
		URL: "https://x/alphaMissense/{alt}.bw",
	}
	if !src.IsPerAlt() || !src.IsBBISource() {
		t.Fatalf("predicates: perAlt=%v bbi=%v", src.IsPerAlt(), src.IsBBISource())
	}
	files := cfg.ResolveSourceFiles(src)
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4 (a,c,g,t)", len(files))
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Alt] = true
		if f.IndexPath != "" {
			t.Errorf("BBI file should have no index path: %+v", f)
		}
	}
	for _, a := range []string{"a", "c", "g", "t"} {
		if !got[a] {
			t.Errorf("missing alt %q in %+v", a, files)
		}
	}
}

// TestPerAltValidation: {alt} is bigwig/bigbed-only and can't combine with chroms.
func TestPerAltValidation(t *testing.T) {
	bad := &Snapshot{Sources: []Source{{
		Name: "x", Version: "1", Format: "vcf", URL: "https://x/{alt}.vcf.gz",
	}}}
	if err := bad.validate(); err == nil {
		t.Error("expected error: {alt} on a non-bigwig/bigbed source")
	}
	ok := &Snapshot{Sources: []Source{{
		Name: "am", Version: "1", Format: "bigwig", URL: "https://x/{alt}.bw",
		Annotations: []Annotation{{Name: "am", Type: "numeric"}},
	}}}
	if err := ok.validate(); err != nil {
		t.Errorf("valid per-alt bigwig should pass: %v", err)
	}
}

// A container binds {datadir} and apptainer runs a .sif from a real path, so
// neither can be composed from an object-store cache. Before this, an s3
// cache_dir produced "s3:/bucket/..." — filepath.Join collapses the scheme —
// which is neither a locator nor a path.
func TestToolPathsStayLocal(t *testing.T) {
	vep := Tool{Name: "vep", Version: "115", Image: "docker://ensemblorg/ensembl-vep:release_115.0"}

	t.Run("remote cache falls back to data_dir", func(t *testing.T) {
		c := &Config{DataDir: "/var/lib/varhub/data", CacheDir: "s3://vh-sources/prod"}
		c.SetBaseDir("/etc/varhub")

		data := c.ResolveToolData(vep)
		if !filepath.IsAbs(data) || strings.Contains(data, "s3:") {
			t.Errorf("ResolveToolData = %q; want a local absolute path", data)
		}
		if want := "/var/lib/varhub/data/tools/vep/115"; data != want {
			t.Errorf("ResolveToolData = %q, want %q", data, want)
		}
		img := c.ResolveToolImage(vep)
		if strings.Contains(img, "s3:") {
			t.Errorf("ResolveToolImage = %q; want a local path", img)
		}
	})

	t.Run("local cache is unchanged", func(t *testing.T) {
		c := &Config{DataDir: "/var/lib/varhub/data", CacheDir: "/mnt/sources"}
		c.SetBaseDir("/etc/varhub")
		if want, got := "/mnt/sources/tools/vep/115", c.ResolveToolData(vep); got != want {
			t.Errorf("ResolveToolData = %q, want %q", got, want)
		}
	})

	t.Run("tool_dir wins", func(t *testing.T) {
		c := &Config{DataDir: "/var/lib/varhub/data", CacheDir: "s3://vh/prod", ToolDir: "/fast/tools"}
		c.SetBaseDir("/etc/varhub")
		if want, got := "/fast/tools/tools/vep/115", c.ResolveToolData(vep); got != want {
			t.Errorf("ResolveToolData = %q, want %q", got, want)
		}
	})
}
