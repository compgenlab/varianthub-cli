package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// newTestEditor builds an editModel over a fresh temp VARHUB_HOME with an empty
// snapshot "s". It drives the model's data methods directly (no TTY/rendering).
func newTestEditor(t *testing.T) *editModel {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VARHUB_HOME", dir)
	cfgPath := filepath.Join(dir, "config.toml")
	body := "data_dir = \"data\"\nannotations_dir = \".\"\ndefault_snapshot = \"s\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// An empty snapshot manifest the editor's saveSource will add refs to.
	if err := config.WriteSnapshotConfig(cfg.SnapshotFile("s"), &config.SnapshotConfig{Assembly: "GRCh38"}); err != nil {
		t.Fatal(err)
	}
	return &editModel{cfg: cfg, cfgPath: cfgPath, width: 80, height: 24, curSnap: "s"}
}

func TestEditorSaveAndReloadSource(t *testing.T) {
	m := newTestEditor(t)

	// New source with a nested annotation → save → reload.
	m.startNewSource()
	m.curSource.Name = "clinvar"
	m.curSource.Version = "2026-01"
	m.curSource.Format = "vcf"
	m.curSource.URL = "https://x/clinvar.vcf.gz"
	m.curSource.Annotations = append(m.curSource.Annotations,
		config.Annotation{Name: "clinvar_sig", Field: "CLNSIG", Type: "categorical"})
	if err := m.saveSource(); err != nil {
		t.Fatalf("saveSource: %v", err)
	}

	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(snap.Sources) != 1 || snap.Sources[0].ID() != "clinvar:2026-01" {
		t.Fatalf("sources = %+v", snap.Sources)
	}
	if len(snap.Annotations) != 1 || snap.Annotations[0].Name != "clinvar_sig" || snap.Annotations[0].Source != "clinvar" {
		t.Fatalf("annotations = %+v", snap.Annotations)
	}

	// Re-open the written fragment and confirm the working source round-trips.
	m.openSource(m.curPath)
	if m.curSource == nil || m.curSource.Name != "clinvar" {
		t.Fatalf("openSource = %+v", m.curSource)
	}
}

func TestEditorSaveSourceMissingFields(t *testing.T) {
	m := newTestEditor(t)
	m.startNewSource() // no name/version/url
	if err := m.saveSource(); err == nil {
		t.Fatal("expected a missing-required-field error")
	}
}

func TestEditorTabColsClearedForVCF(t *testing.T) {
	m := newTestEditor(t)
	m.startNewSource()
	m.curSource.Name, m.curSource.Version, m.curSource.URL = "x", "1", "https://x"
	m.curSource.Format = "vcf"
	m.refColStr, m.altColStr = "3", "4" // set, but format is vcf → must be dropped
	if err := m.saveSource(); err != nil {
		t.Fatal(err)
	}
	snap, _ := m.cfg.LoadSnapshot("s")
	if snap.Sources[0].RefCol != 0 || snap.Sources[0].AltCol != 0 {
		t.Errorf("vcf source kept ref/alt cols: %+v", snap.Sources[0])
	}
}

func TestEditorBuiltinsAddAndRemove(t *testing.T) {
	m := newTestEditor(t)
	m.toBuiltins("s") // find-or-create the builtin fragment
	m.appendBuiltin("tstv", "")
	m.appendBuiltin("tags", "PANEL:v1")
	if err := m.saveBuiltins(); err != nil {
		t.Fatal(err)
	}
	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := map[string]string{}
	for _, a := range snap.Annotations {
		got[a.Source] = a.Args // builtin source == builtin name
	}
	if _, ok := got["tstv"]; !ok {
		t.Errorf("tstv missing: %+v", snap.Annotations)
	}
	if got["tags"] != "PANEL:v1" {
		t.Errorf("tags args = %q, want PANEL:v1", got["tags"])
	}

	// Remove tstv, keep tags.
	m.removeBuiltin("tstv")
	if err := m.saveBuiltins(); err != nil {
		t.Fatal(err)
	}
	snap, _ = m.cfg.LoadSnapshot("s")
	if len(snap.Annotations) != 1 || snap.Annotations[0].Source != "tags" {
		t.Errorf("after remove = %+v", snap.Annotations)
	}
}

// TestEditorSnapshotMembersAndDefaults drives the manifest-level checkbox editors:
// membership rewrites the snapshot's sources list, defaults writes
// default_annotations, and dropping a source prunes now-orphaned defaults.
func TestEditorSnapshotDefaults(t *testing.T) {
	m := newTestEditor(t)

	// Add a source with one annotation; saveSource references it from the snapshot.
	m.startNewSource()
	m.curSource.Name, m.curSource.Version, m.curSource.Format = "clinvar", "2026-01", "vcf"
	m.curSource.URL = "https://x/clinvar.vcf.gz"
	m.curSource.Annotations = append(m.curSource.Annotations,
		config.Annotation{Name: "clinvar_sig", Field: "CLNSIG", Type: "categorical"})
	if err := m.saveSource(); err != nil {
		t.Fatalf("saveSource: %v", err)
	}

	// The defaults editor should offer exactly that annotation.
	if names := m.snapshotAnnotationNames(); len(names) != 1 || names[0] != "clinvar_sig" {
		t.Fatalf("annotation options = %v, want [clinvar_sig]", names)
	}

	// Mark it default and persist to the manifest.
	m.defaultAnns = []string{"clinvar_sig"}
	if err := m.saveSnapDefaults(); err != nil {
		t.Fatalf("saveSnapDefaults: %v", err)
	}
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Defaults) != 1 || sc.Defaults[0] != "clinvar_sig" {
		t.Fatalf("manifest defaults = %v, want [clinvar_sig]", sc.Defaults)
	}
	// LoadSnapshot derives Annotation.Default from the manifest.
	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Annotations) != 1 || !snap.Annotations[0].Default {
		t.Fatalf("expected clinvar_sig marked default, got %+v", snap.Annotations)
	}

	// Removing the source via the members editor prunes the orphaned default.
	m.memberSources = nil
	if err := m.saveSnapMembers(); err != nil {
		t.Fatalf("saveSnapMembers: %v", err)
	}
	sc, _ = config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if len(sc.Sources) != 0 {
		t.Errorf("sources not cleared: %v", sc.Sources)
	}
	if len(sc.Defaults) != 0 {
		t.Errorf("orphaned default not pruned: %v", sc.Defaults)
	}
}

func TestEditorViewRenders(t *testing.T) {
	m := newTestEditor(t)
	m.width, m.height = 90, 24
	m.toSnapshots()
	out := m.View()
	for _, want := range []string{"varhub", "snapshots", "back"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshots view missing %q", want)
		}
	}
	// The home menu renders with the three areas.
	m.toHome()
	home := m.View()
	for _, want := range []string{"Config settings", "Sources", "Snapshots", "quit"} {
		if !strings.Contains(home, want) {
			t.Errorf("home view missing %q", want)
		}
	}
	// A source form renders without panic, showing the breadcrumb + first field.
	m.startNewSource()
	m.toSourceForm()
	out = m.View()
	for _, want := range []string{"varhub", "source", "name"} {
		if !strings.Contains(out, want) {
			t.Errorf("source form missing %q", want)
		}
	}
}

// TestEditorLibrarySaveNoSnapshotRef: saving a source in the library (libraryMode, no
// current snapshot) writes the fragment but does NOT add a ref to any snapshot manifest.
func TestEditorLibrarySaveNoSnapshotRef(t *testing.T) {
	m := newTestEditor(t)
	m.libraryMode = true
	m.curSnap = ""
	m.startNewSource()
	m.curSource.Name, m.curSource.Version, m.curSource.Format = "gnomad", "4.1", "vcf"
	m.curSource.URL = "https://x/g.vcf.gz"
	if err := m.saveSource(); err != nil {
		t.Fatalf("saveSource: %v", err)
	}
	if _, err := os.Stat(m.cfg.SourceFile("gnomad", "4.1")); err != nil {
		t.Fatalf("source fragment not written: %v", err)
	}
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Sources) != 0 {
		t.Errorf("library save should not touch the snapshot, but sources = %v", sc.Sources)
	}
}

// TestEditorSourcesBrowse: the library browser lists every local source (data + builtin),
// regardless of snapshot membership, plus the add entries.
func TestEditorSourcesBrowse(t *testing.T) {
	m := newTestEditor(t)
	if err := config.WriteFragment(m.cfg.SourceFile("clinvar", "2026-01"), &config.Snapshot{Sources: []config.Source{{
		Name: "clinvar", Version: "2026-01", Format: "vcf", URL: "https://x/c.vcf.gz",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteFragment(m.cfg.SourceFile("builtins", "1"), &config.Snapshot{Sources: []config.Source{{
		Name: "builtins", Version: "1", Type: "builtin", Annotations: []config.Annotation{{Builtin: "tstv"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	m.toSources()
	if !m.libraryMode {
		t.Error("toSources should set libraryMode")
	}
	var titles []string
	for _, it := range m.list.Items() {
		titles = append(titles, it.(item).title)
	}
	joined := strings.Join(titles, "|")
	for _, want := range []string{"clinvar:2026-01", "builtins:1", "Add source", "Builtins"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sources browser missing %q: %v", want, titles)
		}
	}
}

// TestEditorConfigRoundTrip: the config editor writes config.toml, preserving $VARHUB_HOME
// literals and round-tripping registries + backend.
func TestEditorConfigRoundTrip(t *testing.T) {
	m := newTestEditor(t)
	m.toConfig()
	if m.cfgEdit == nil {
		t.Fatal("toConfig did not load the config")
	}
	m.cfgEdit.DataDir = "$VARHUB_HOME/data"
	m.cfgEdit.Database.Backend = "sqlite"
	m.cfgRegistries = "https://a/r.toml\nhttps://b/r.toml\n"
	if err := m.saveConfig(); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := config.ReadConfigFile(m.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if raw.DataDir != "$VARHUB_HOME/data" {
		t.Errorf("data_dir literal not preserved: %q", raw.DataDir)
	}
	if len(raw.Registries) != 2 || raw.Registries[0] != "https://a/r.toml" {
		t.Errorf("registries = %v", raw.Registries)
	}
	if raw.Database.Backend != "sqlite" {
		t.Errorf("backend = %q", raw.Database.Backend)
	}
}

// TestEditorSourceMenuSelectDefaults: opening an existing source in a snapshot shows a
// menu; "select default annotations" toggles only THIS source's names in the manifest's
// default_annotations, leaving another source's defaults intact.
func TestEditorSourceMenuSelectDefaults(t *testing.T) {
	m := newTestEditor(t)

	// A source with two annotations, referenced by the snapshot; snapshot already
	// defaults an annotation belonging to a *different* source ("other_x").
	if err := config.WriteFragment(m.cfg.SourceFile("clinvar", "2026-01"), &config.Snapshot{Sources: []config.Source{{
		Name: "clinvar", Version: "2026-01", Format: "vcf", URL: "https://x/c.vcf.gz",
		Annotations: []config.Annotation{
			{Name: "clinvar_sig", Field: "CLNSIG", Type: "categorical"},
			{Name: "clinvar_pos", Type: "flag"},
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteSnapshotConfig(m.cfg.SnapshotFile("s"), &config.SnapshotConfig{
		Assembly: "GRCh38", Sources: []string{"clinvar:2026-01"}, Defaults: []string{"other_x"},
	}); err != nil {
		t.Fatal(err)
	}

	// Open the source (snapshot mode) → menu is shown.
	m.curSnap, m.libraryMode = "s", false
	m.openSource(m.cfg.SourceFile("clinvar", "2026-01"))
	m.toSourceMenu()
	kinds := map[string]bool{}
	for _, it := range m.list.Items() {
		kinds[it.(item).kind] = true
	}
	if !kinds["editsrc"] || !kinds["seldef"] {
		t.Fatalf("source menu missing edit/select items: %v", kinds)
	}

	// Select clinvar_sig as a default for this source, then save.
	m.toSourceDefaults()
	m.defaultAnns = []string{"clinvar_sig"}
	if err := m.saveSourceDefaults(); err != nil {
		t.Fatalf("saveSourceDefaults: %v", err)
	}
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if err != nil {
		t.Fatal(err)
	}
	got := stringSet(sc.Defaults)
	if !got["clinvar_sig"] {
		t.Errorf("clinvar_sig not marked default: %v", sc.Defaults)
	}
	if !got["other_x"] {
		t.Errorf("another source's default (other_x) was dropped: %v", sc.Defaults)
	}
	if got["clinvar_pos"] {
		t.Errorf("clinvar_pos should not be default: %v", sc.Defaults)
	}
}

// TestEditorLibraryMenuEditAnnotationsPersists: in the library, the source menu offers
// "edit annotations", and adding one persists to the fragment immediately (no form Save).
func TestEditorLibraryMenuEditAnnotationsPersists(t *testing.T) {
	m := newTestEditor(t)
	if err := config.WriteFragment(m.cfg.SourceFile("cadd", "1.7"), &config.Snapshot{Sources: []config.Source{{
		Name: "cadd", Version: "1.7", Format: "tab", RefCol: 3, AltCol: 4, URL: "https://x/cadd.gz",
	}}}); err != nil {
		t.Fatal(err)
	}
	m.libraryMode, m.curSnap = true, ""
	m.openSource(m.cfg.SourceFile("cadd", "1.7"))
	m.toSourceMenu()
	// library menu → "edit annotations"
	m.annFromForm = false
	m.curSource.Annotations = append(m.curSource.Annotations,
		config.Annotation{Name: "cadd_phred", Field: "6", Type: "numeric"})
	m.persistCurSource()

	frag, err := config.ReadFragment(m.cfg.SourceFile("cadd", "1.7"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frag.Sources) != 1 || len(frag.Sources[0].Annotations) != 1 ||
		frag.Sources[0].Annotations[0].Name != "cadd_phred" {
		t.Fatalf("annotation not persisted to fragment: %+v", frag.Sources)
	}
}
