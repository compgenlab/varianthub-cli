// Package faidx builds the indexes a tool needs to random-access a FASTA.
//
// A wrapper over cghts, which owns these formats alongside the readers that
// consume them (seqio.IndexedFastaReader, bgzf.LoadGZIndex). It existed as a
// full implementation here only until that landed; keeping two would mean two
// things that can disagree about a byte layout.
package faidx

import (
	"github.com/compgenlab/cghts/seqio"
)

// Entry is one record of a .fai index.
type Entry = seqio.FaiEntry

// Build writes <path>.fai, and <path>.gzi when path is BGZF, returning the
// entries written.
//
// Plain gzip is refused: a .fai addresses uncompressed offsets and only a
// container that can seek to one can use it, so the index would be unusable
// rather than merely imperfect. Recompress with `varhub bgzip` first.
func Build(path string) ([]Entry, error) { return seqio.BuildFastaIndex(path) }
