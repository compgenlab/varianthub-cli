package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// TestContainerMapping: a container step binds each host dir to a fixed /varhub/*
// mountpoint and renders the placeholders to those in-container paths (host-
// independent, so the engine never recreates a deep host path inside the image).
func TestContainerMapping(t *testing.T) {
	// Ref/input dirs are only bound when they exist on the host (so tool setup with
	// no reference yet doesn't fail on a missing mount), so give them real dirs.
	refDir := t.TempDir()
	inDir := t.TempDir()
	p := Params{
		Datadir: "/home/u/deep/cache/tools/vep/113",
		Workdir: "/tmp/varhub-xyz",
		Ref:     filepath.Join(refDir, "GRCh38.fa"),
		Input:   filepath.Join(inDir, "in.vcf"),
		Output:  "/tmp/varhub-xyz/vep.vcf.gz",
		Image:   "/img/vep.sif",
	}
	repl, binds := containerMapping(config.Tool{}, p)

	wantBinds := []string{
		"-B", "/home/u/deep/cache/tools/vep/113:/varhub/data",
		"-B", "/tmp/varhub-xyz:/varhub/work",
		"-B", refDir + ":/varhub/ref",
		"-B", inDir + ":/varhub/in",
	}
	if strings.Join(binds, " ") != strings.Join(wantBinds, " ") {
		t.Errorf("binds = %v\nwant %v", binds, wantBinds)
	}

	got := repl.Replace("vep -i {input} -o {output} --dir_cache {datadir} --fasta {ref} --work {workdir}")
	want := "vep -i /varhub/in/in.vcf -o /varhub/work/vep.vcf.gz --dir_cache /varhub/data --fasta /varhub/ref/GRCh38.fa --work /varhub/work"
	if got != want {
		t.Errorf("render =\n %q\nwant\n %q", got, want)
	}
}

// TestContainerMappingMissingRef: a reference/input whose dir doesn't exist is NOT
// bound (so tool setup won't fail on a not-yet-configured FASTA), but {ref} still
// renders its in-container path.
func TestContainerMappingMissingRef(t *testing.T) {
	p := Params{
		Datadir: "/cache/vep",
		Workdir: "/tmp/wd",
		Ref:     "/does/not/exist/GRCh38.fa",
		Image:   "/img/vep.sif",
	}
	repl, binds := containerMapping(config.Tool{}, p)
	for _, b := range binds {
		if strings.Contains(b, "/varhub/ref") {
			t.Errorf("missing ref dir should not be bound, got %v", binds)
		}
	}
	if got := repl.Replace("INSTALL.pl -c {datadir}"); got != "INSTALL.pl -c /varhub/data" {
		t.Errorf("setup render = %q", got)
	}
	// {ref} still maps to the in-container path (a step that uses it fails clearly).
	if got := repl.Replace("--fasta {ref}"); got != "--fasta /varhub/ref/GRCh38.fa" {
		t.Errorf("ref render = %q", got)
	}
}

// TestContainerMappingRelativeInput: a RELATIVE input path (e.g. an input VCF given
// as `sub/dir/in.vcf.gz`) must bind an ABSOLUTE host dir. The container engine
// resolves a relative -B source against its own CWD (the tool workdir), not
// varhub's, so a relative bind would mount a path that doesn't exist there.
func TestContainerMappingRelativeInput(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := os.MkdirAll("sub/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	p := Params{
		Datadir: "/cache/vep", Workdir: "/tmp/wd",
		Input: "sub/dir/in.vcf.gz", Image: "/img/vep.sif",
	}
	_, binds := containerMapping(config.Tool{}, p)
	joined := strings.Join(binds, " ")
	want := "-B " + filepath.Join(cwd, "sub", "dir") + ":/varhub/in"
	if !strings.Contains(joined, want) {
		t.Errorf("relative input must bind an ABSOLUTE host dir;\n want %q\n in   %q", want, joined)
	}
	if strings.Contains(joined, "-B sub/dir:") {
		t.Errorf("bind is relative (would break the container mount): %v", binds)
	}
}

// TestStageAssets: a declared asset (co-located in AssetDir) is staged into the
// workdir and a host step runs it explicitly as {workdir}/<name> — no PATH reliance.
func TestStageAssets(t *testing.T) {
	dir := t.TempDir()
	assetDir := filepath.Join(dir, "frag")
	workdir := filepath.Join(dir, "wd")
	for _, d := range []string{assetDir, workdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "ran")
	// A helper script co-located with the fragment; the step invokes it by workdir path.
	if err := os.WriteFile(filepath.Join(assetDir, "helper.sh"),
		[]byte("#!/bin/sh\necho hi > \""+marker+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := config.Tool{
		Name: "t", Version: "1",
		Assets: []string{"helper.sh"},
		Setup:  []config.Step{{Name: "run", Run: "sh {workdir}/helper.sh"}},
	}
	if err := Setup(context.Background(), tl, Params{Workdir: workdir, AssetDir: assetDir}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "helper.sh")); err != nil {
		t.Errorf("asset not staged into workdir: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("staged asset did not run (marker missing): %v", err)
	}
	// A declared-but-missing asset is a clear error.
	bad := config.Tool{Name: "t", Version: "1", Assets: []string{"nope.sh"},
		Setup: []config.Step{{Run: "true"}}}
	if err := Setup(context.Background(), bad, Params{Workdir: workdir, AssetDir: assetDir}); err == nil {
		t.Error("expected error for missing asset")
	}
}

// TestSetupLocal: a setup step resolves {datadir} and runs (installing tool data).
func TestSetupLocal(t *testing.T) {
	dir := t.TempDir()
	datadir := filepath.Join(dir, "data")
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		t.Fatal(err)
	}
	tl := config.Tool{
		Name: "vep", Version: "112",
		Setup: []config.Step{{Name: "install", Run: "touch {datadir}/installed"}},
	}
	if err := Setup(context.Background(), tl, Params{Datadir: datadir, Workdir: dir}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(datadir, "installed")); err != nil {
		t.Errorf("setup did not create {datadir}/installed: %v", err)
	}
}

// prebuiltTab writes a tiny indexed tab file the fake tool steps will copy.
func prebuiltTab(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "pre.tab.gz")
	w := tabix.NewWriter(p, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	if err := w.Write("chr1\t100\tA\tG\t0.5"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// copyStep is a step that "produces" the output by copying the prebuilt file.
func copyStep(pre string) config.Step {
	return config.Step{Name: "produce", Run: "cp " + pre + " {output}; cp " + pre + ".tbi {output}.tbi"}
}

func runOK(t *testing.T, tl config.Tool, dir string) {
	t.Helper()
	out := filepath.Join(dir, "out.tab.gz")
	if err := Run(context.Background(), tl, Params{Output: out, Workdir: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, err := tabix.NewReader(out)
	if err != nil {
		t.Fatalf("output not a valid tabix file: %v", err)
	}
	r.Close()
}

func TestRunLocal(t *testing.T) {
	dir := t.TempDir()
	pre := prebuiltTab(t, dir)
	runOK(t, config.Tool{
		Name: "x", Version: "1", Format: "tab",
		Steps: []config.Step{copyStep(pre)},
	}, dir)
}

func TestContainerStepNeedsImage(t *testing.T) {
	dir := t.TempDir()
	err := Run(context.Background(), config.Tool{
		Name: "x", Version: "1", Format: "tab",
		Steps: []config.Step{{Run: "true", Container: true}},
	}, Params{Output: filepath.Join(dir, "o.gz"), Workdir: dir})
	if err == nil {
		t.Fatal("expected error for a container step without an image")
	}
}

func TestNoSteps(t *testing.T) {
	if err := Run(context.Background(), config.Tool{Name: "x", Version: "1"}, Params{}); err == nil {
		t.Fatal("expected error for a tool with no steps")
	}
}

// TestMissingAssets: a helper script read via {workdir}/x that isn't declared in
// `assets` (nor produced by a step) is flagged; a declared asset and a step-produced
// intermediate are not.
func TestMissingAssets(t *testing.T) {
	tl := config.Tool{
		Output: "vep.vcf.gz",
		Assets: []string{"expand_vep_vcf.py"}, // declared
		Steps: []config.Step{
			{Run: "vep -i {input} -o {workdir}/vep.vcf --fasta {ref}"}, // produces vep.vcf
			{Run: "python3 {workdir}/expand_vep_vcf.py < {workdir}/vep.vcf | python3 {workdir}/worst.py | varhub bgzip > {output}"},
		},
	}
	got := missingAssets(tl, tl.Steps)
	// expand_vep_vcf.py is declared, vep.vcf is produced by step 1 → only worst.py is missing.
	if len(got) != 1 || got[0] != "worst.py" {
		t.Fatalf("missingAssets = %v, want [worst.py]", got)
	}

	// Declaring it clears the warning.
	tl.Assets = append(tl.Assets, "worst.py")
	if got := missingAssets(tl, tl.Steps); len(got) != 0 {
		t.Errorf("after declaring, missingAssets = %v, want none", got)
	}
}
