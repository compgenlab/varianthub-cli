package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// checkAssets reports the helper files a source declares but does not have.
//
// Preflighted for the same reason required software is: a build recipe or tool
// step that names a script cannot run without it, and finding that out after
// pulling a multi-gigabyte image — or partway through an install that has
// already been going for an hour — costs everything spent up to that point to
// learn something knowable in advance.
//
// URL assets are skipped: varhub fetches those when the step runs, so their
// absence here is expected rather than a problem.
func checkAssets(cfg *config.Config, label string, name, version string, assets []string) error {
	dir := cfg.SourceDir(name, version)
	var missing []string
	for _, a := range assets {
		if a == "" || strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, a)); err != nil {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Naming the directory matters: the fix is to put the file there, and the
	// path is not guessable from the manifest alone.
	return fmt.Errorf("%s: helper file(s) not found in %s: %s\n"+
		"  they are declared by the source and must sit beside its manifest;\n"+
		"  a registry copy should have shipped them, so re-pull the source or add them by hand",
		label, dir, strings.Join(missing, ", "))
}
