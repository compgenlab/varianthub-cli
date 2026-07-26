package annotate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// TestGroupedSourceMultiField: two annotations on ONE tab source are served by a
// single grouped annotator (one reader, one query per record) and must both appear
// with correct values in the output — the multi-field grouping path introduced in
// BuildPipeline / SourceAnnotators.
func TestGroupedSourceMultiField(t *testing.T) {
	dir := t.TempDir()
	tabPath := filepath.Join(dir, "src.tab.gz")
	// columns: chrom pos ref alt score label
	w := tabix.NewWriter(tabPath, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, l := range []string{
		"chr1\t100\tA\tG\t0.10\thot",
		"chr1\t200\tC\tT\t0.20\tcold",
	} {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	snap := &config.Snapshot{
		Name:    "r",
		Sources: []config.Source{{Name: "src", Format: "tab", RefCol: 3, AltCol: 4, LocalPath: tabPath}},
		Annotations: []config.Annotation{
			{Name: "SCORE", Source: "src", Field: "5", Type: "numeric"},
			{Name: "LABEL", Source: "src", Field: "6", Type: "categorical"},
		},
	}

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(parallelInputVCF), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.vcf")
	if err := AnnotateVCFSnapshot(context.Background(), &config.Config{}, snap, in, out, 1, false, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"SCORE=0.10", "LABEL=hot", "SCORE=0.20", "LABEL=cold"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q; got:\n%s", want, s)
		}
	}
	// Both fields must be declared exactly once in the header.
	for _, def := range []string{"##INFO=<ID=SCORE,", "##INFO=<ID=LABEL,"} {
		if n := strings.Count(s, def); n != 1 {
			t.Errorf("header has %d %q defs, want 1", n, def)
		}
	}
}
