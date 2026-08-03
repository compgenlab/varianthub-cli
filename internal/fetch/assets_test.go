package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// A declared helper file that is not there must be reported before anything
// expensive happens — not after a multi-gigabyte image pull, which is where it
// surfaced before.
func TestCheckAssets(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{AnnotationsDir: filepath.Join(home, "annotations")}
	cfg.SetBaseDir(home)

	dir := cfg.SourceDir("vep", "113")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Missing: named, and the directory to put it in is named too, since that
	// path is not guessable from the manifest.
	err := checkAssets(cfg, "vep:113", "vep", "113", []string{"expand.py", "worst.py"})
	if err == nil {
		t.Fatal("a missing asset was not reported")
	}
	for _, want := range []string{"expand.py", "worst.py", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// Present: no complaint.
	for _, n := range []string{"expand.py", "worst.py"} {
		if wErr := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); wErr != nil {
			t.Fatal(wErr)
		}
	}
	if err := checkAssets(cfg, "vep:113", "vep", "113", []string{"expand.py", "worst.py"}); err != nil {
		t.Errorf("present assets reported missing: %v", err)
	}

	// A URL asset is fetched when the step runs, so its absence here is not a
	// problem and must not block the download.
	if err := checkAssets(cfg, "vep:113", "vep", "113",
		[]string{"https://example.org/remote.py"}); err != nil {
		t.Errorf("a URL asset was treated as missing: %v", err)
	}

	// Nothing declared, nothing to check.
	if err := checkAssets(cfg, "x:1", "x", "1", nil); err != nil {
		t.Errorf("no assets = %v", err)
	}
}
