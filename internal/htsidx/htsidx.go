// Package htsidx builds tabix indexes with the error handling every call site
// needs.
//
// It exists for two reasons that are easy to get wrong independently at each of
// the four places varhub indexes a file:
//
//   - A failed build must not leave an older index behind. tabix writes nothing
//     on failure, but a sidecar from a previous run survives, and every
//     "is this source complete?" check in varhub tests only for presence.
//   - "not coordinate sorted" needs to say what to do about it. The underlying
//     error names the offending line precisely but not the fix, and the fix
//     differs depending on which ordering rule was broken.
package htsidx

import (
	"errors"
	"fmt"
	"os"

	"github.com/compgenlab/cghts/htsio/tabix"
)

// WriteIndex builds a tabix index for target using opts.
//
// On failure any pre-existing .tbi/.csi beside target is removed. That is
// deliberate and slightly destructive: whichever way you read a failed rebuild,
// the old index cannot be trusted. Either target changed since it was written,
// which is what re-indexing means, or target is unsorted and the old index was
// built over the same unsorted data by a tabix that used to accept it. Leaving
// it is worse than removing it — a stale index is structurally valid, so it
// passes every check varhub makes and then silently resolves queries to the
// wrong offsets.
func WriteIndex(opts *tabix.WriterOpts, target string) error {
	if err := tabix.NewIndexWriter(opts).WriteIndex(target); err != nil {
		removed := discardIndex(target)
		return explain(err, removed)
	}
	return nil
}

// discardIndex removes the index sidecars beside target, reporting those that
// were actually there.
func discardIndex(target string) []string {
	var removed []string
	for _, ext := range []string{".tbi", ".csi"} {
		if err := os.Remove(target + ext); err == nil {
			removed = append(removed, ext)
		}
	}
	return removed
}

// explain adds a remediation hint to an unsorted-input error, and says so when
// an existing index was discarded. Other errors pass through untouched.
func explain(err error, removed []string) error {
	var u *tabix.UnsortedError
	if !errors.As(err, &u) {
		return err
	}

	// The two rules fail for different reasons and have different fixes, so a
	// single generic "sort your input" would send half of all callers looking in
	// the wrong place.
	hint := "positions must ascend within a contig; equal positions are fine, so multiallelic sites split across lines are not the problem"
	if u.Revisit {
		hint = "each contig must appear as one contiguous block, though the order of the contigs themselves does not matter"
	}

	var note string
	if len(removed) > 0 {
		note = fmt.Sprintf("\nThe existing %s was removed: it could not be trusted after a failed rebuild.",
			removed[0])
	}

	return fmt.Errorf("%w (%s).\nSort before indexing, e.g.\n"+
		"  (grep '^#' in; grep -v '^#' in | sort -k1,1 -k2,2n) | varhub bgzip -p <format> -o out.gz%s",
		u, hint, note)
}
