package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/engine"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// annotateResultFixture: one locus with a categorical + a numeric annotation.
func annotateResultFixture() ([]model.Locus, []config.Annotation, engine.AnnotateResult) {
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	selected := []config.Annotation{
		{Name: "clinvar_sig", Type: "categorical"},
		{Name: "gnomad_af", Type: "numeric"},
	}
	res := engine.AnnotateResult{
		Version: "2026-06", Novel: 1,
		ByLocus: map[string][]model.AnnRow{
			l.Key(): {
				{Locus: l, DataSource: "clinvar:1", Key: "clinvar_sig", Value: model.Text("Pathogenic")},
				{Locus: l, DataSource: "gnomad:1", Key: "gnomad_af", Value: model.Number(0.001)},
			},
		},
	}
	return []model.Locus{l}, selected, res
}

func TestFormatResultsTab(t *testing.T) {
	loci, selected, res := annotateResultFixture()
	var buf bytes.Buffer
	if err := formatResults(&buf, "tab", loci, selected, res); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if lines[0] != "#chrom\tpos\tref\talt\tclinvar_sig\tgnomad_af" {
		t.Errorf("header = %q", lines[0])
	}
	if lines[1] != "chr1\t100\tA\tG\tPathogenic\t0.001" {
		t.Errorf("row = %q", lines[1])
	}
}

func TestFormatResultsJSON(t *testing.T) {
	loci, selected, res := annotateResultFixture()
	var buf bytes.Buffer
	if err := formatResults(&buf, "json", loci, selected, res); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(out) != 1 || out[0]["chrom"] != "chr1" {
		t.Fatalf("json = %v", out)
	}
	anns := out[0]["annotations"].(map[string]any)
	if anns["clinvar_sig"] != "Pathogenic" {
		t.Errorf("clinvar_sig = %v", anns["clinvar_sig"])
	}
	if anns["gnomad_af"].(float64) != 0.001 { // numeric stays a JSON number
		t.Errorf("gnomad_af = %v (want number 0.001)", anns["gnomad_af"])
	}
}

func TestFormatResultsText(t *testing.T) {
	loci, selected, res := annotateResultFixture()
	var buf bytes.Buffer
	if err := formatResults(&buf, "text", loci, selected, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"chr1:100:A:G", "clinvar_sig", "Pathogenic", "snapshot 2026-06"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}
