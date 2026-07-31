package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// Inventory has to answer for a remote cache, where there is nothing to walk.
// That is the whole reason it resolves locators instead of scanning.
func TestInventoryRemote(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "src.vcf.gz"), false)
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg("s3://bucket/prefix")
	src := config.Source{Name: "src", Version: "1", Format: "vcf", URL: ts.URL + "/src.vcf.gz"}
	ctx := context.Background()

	// Nothing provisioned yet.
	if files, err := Inventory(ctx, cfg, src); err != nil || len(files) != 0 {
		t.Fatalf("Inventory before download = %v, %v; want empty", files, err)
	}

	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	files, err := Inventory(ctx, cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, f := range files {
		got[f.Path] = f.SizeBytes
	}
	for _, want := range []string{"src/1/src.vcf.gz", "src/1/src.vcf.gz.tbi"} {
		if got[want] == 0 {
			t.Errorf("missing or zero-length %q; got %v", want, got)
		}
	}
	if len(files) != 2 {
		t.Errorf("got %d files, want 2: %v", len(files), got)
	}
	// Paths are relative to the cache root, so they read the same for a bucket
	// as for a directory.
	for _, f := range files {
		if len(f.Path) > 0 && f.Path[0] == 's' && f.Path[:2] == "s3" {
			t.Errorf("path %q is a full locator, not relative to the cache root", f.Path)
		}
	}
	_ = store
}

// A GTF is queried through its converted copy, which has a different name than
// the download — so an inventory that only looked at the source URL would miss
// the file that actually matters.
func TestInventoryCoversConvertedGTF(t *testing.T) {
	useStub(t)
	srvDir := t.TempDir()
	gtf := "chr1\ttest\tgene\t50\t500\t.\t+\t.\tgene_id \"G1\"; gene_name \"AAA\";\n"
	if err := writeFile(filepath.Join(srvDir, "genes.gtf"), gtf); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg("s3://bucket/prefix")
	src := config.Source{Name: "genes", Version: "1", Format: "gtf", URL: ts.URL + "/genes.gtf"}
	ctx := context.Background()
	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	files, err := Inventory(ctx, cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Path] = true
	}
	if !seen["genes/1/genes.gtf.gz"] || !seen["genes/1/genes.gtf.gz.tbi"] {
		t.Errorf("converted GTF pair not reported; got %v", seen)
	}
	// The original was pruned, so it must not be listed as present.
	if seen["genes/1/genes.gtf"] {
		t.Error("the pruned original is still reported as present")
	}
}

// Builtins occupy nothing.
func TestInventoryBuiltinIsEmpty(t *testing.T) {
	cfg := remoteCfg("s3://bucket/prefix")
	src := config.Source{Name: "builtins", Version: "1", Type: "builtin"}
	files, err := Inventory(context.Background(), cfg, src)
	if err != nil || len(files) != 0 {
		t.Errorf("Inventory(builtin) = %v, %v; want empty", files, err)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
