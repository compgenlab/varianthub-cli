// Package faidx builds the indexes a tool needs to random-access a FASTA: the
// .fai every reader uses, and the .gzi that makes a BGZF file seekable by
// uncompressed offset.
//
// cghts can read both (seqio.IndexedFastaReader, bgzf.LoadGZIndex) but writes
// neither, and the alternative was putting htslib in every image that provisions
// a reference. Keeping it here means one implementation of these formats in the
// stack rather than two that can disagree.
package faidx

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// Entry is one record of a .fai index.
type Entry struct {
	Name      string // sequence name, up to the first whitespace
	Length    int64  // bases
	Offset    int64  // uncompressed byte offset of the first base
	LineBases int64  // bases per line
	LineWidth int64  // bytes per line, including the newline
}

// Build writes <path>.fai, and <path>.gzi when path is BGZF.
//
// Returns the entries written, so a caller can report what it indexed without
// reading the file back.
//
// Plain gzip is refused rather than handled. A .fai indexes uncompressed
// offsets, so it is only useful with a container that can seek to one — BGZF
// can, plain gzip cannot, and writing an index that no reader can use would be
// worse than saying so. Recompress with `varhub bgzip` first.
func Build(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var entries []Entry
	if bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		ir := newIndexingReader(f)
		if entries, err = index(ir); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := writeGZI(path+".gzi", ir.blocks); err != nil {
			return nil, err
		}
	} else if entries, err = index(f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if err := writeFAI(path+".fai", entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// block is one BGZF block's position: where it starts in the file, and what
// uncompressed offset its first byte carries.
type block struct{ compressed, uncompressed int64 }

// indexingReader decompresses a BGZF stream, recording block boundaries as it
// goes.
//
// A reader rather than a buffer: this indexes genomes, and holding the
// decompressed stream to find boundaries is ~3 GB for GRCh38 — which is how the
// worker got OOM-killed. Memory here is one block at a time.
type indexingReader struct {
	br      *bufio.Reader
	blocks  []block
	buf     []byte
	coffset int64
	uoffset int64
	first   bool
	done    bool
	err     error
}

func newIndexingReader(r io.Reader) *indexingReader {
	return &indexingReader{br: bufio.NewReaderSize(r, 1<<20), first: true}
}

func (r *indexingReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			if r.err != nil {
				return 0, r.err
			}
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.done = true
			if err != io.EOF {
				r.err = err
				return 0, err
			}
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *indexingReader) next() error {
	hdr := make([]byte, 18)
	n, err := io.ReadFull(r.br, hdr)
	if err == io.EOF || (err == io.ErrUnexpectedEOF && n == 0) {
		return io.EOF
	}
	if err != nil {
		return fmt.Errorf("read block header at %d: %w", r.coffset, err)
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		return fmt.Errorf("not BGZF: block at %d has no gzip magic", r.coffset)
	}
	if hdr[3]&0x04 == 0 {
		return fmt.Errorf("not BGZF: plain gzip — recompress with `varhub bgzip`")
	}
	// The BC subfield carries BSIZE-1, the total block length, and is the first
	// extra subfield in every BGZF block — which is what the fixed 18-byte
	// header read above assumes.
	if hdr[12] != 'B' || hdr[13] != 'C' {
		return fmt.Errorf("not BGZF: block at %d has no BC subfield", r.coffset)
	}
	bsize := int(binary.LittleEndian.Uint16(hdr[16:18])) + 1
	if bsize <= 18 {
		return fmt.Errorf("not BGZF: block at %d declares length %d", r.coffset, bsize)
	}
	rest := make([]byte, bsize-18)
	if _, err := io.ReadFull(r.br, rest); err != nil {
		return fmt.Errorf("read block at %d: %w", r.coffset, err)
	}
	whole := make([]byte, 0, bsize)
	whole = append(append(whole, hdr...), rest...)

	zr, err := gzip.NewReader(bytes.NewReader(whole))
	if err != nil {
		return fmt.Errorf("block at %d: %w", r.coffset, err)
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return fmt.Errorf("block at %d: %w", r.coffset, err)
	}
	if len(data) == 0 {
		return io.EOF // the EOF marker indexes nothing
	}
	if !r.first {
		r.blocks = append(r.blocks, block{compressed: r.coffset, uncompressed: r.uoffset})
	}
	r.first = false

	r.buf = data
	r.coffset += int64(bsize)
	r.uoffset += int64(len(data))
	return nil
}

// index walks a FASTA and records where each sequence starts.
//
// Rejects a record whose lines are not uniform, because .fai cannot describe
// one: the format stores a single line length per sequence and computes an
// offset arithmetically from it, so a ragged record would silently produce
// wrong coordinates rather than a failure.
func index(r io.Reader) ([]Entry, error) {
	var (
		out    []Entry
		cur    *Entry
		pos    int64
		lineNo int64
		ragged bool
	)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		// A read error is not end-of-input: treating them alike turns a
		// truncated stream into a short index that looks complete, and swallows
		// "this is not BGZF" entirely.
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(line) == 0 && err != nil {
			break
		}
		lineNo++
		width := int64(len(line))
		trimmed := bytes.TrimRight(line, "\r\n")

		if len(trimmed) > 0 && trimmed[0] == '>' {
			if cur != nil {
				out = append(out, *cur)
			}
			name := strings.Fields(string(trimmed[1:]))
			n := ""
			if len(name) > 0 {
				n = name[0]
			}
			cur = &Entry{Name: n, Offset: pos + width}
			ragged = false
			pos += width
			if err != nil {
				break
			}
			continue
		}
		if cur == nil {
			pos += width
			if err != nil {
				break
			}
			continue // leading junk before any header
		}

		bases := int64(len(trimmed))
		// A short line ends the record. Anything after one is ragged whatever
		// its length, so the check is "did a short line already happen", not "is
		// this line a different length" — 8/3/8 passes the second and fails the
		// first.
		if ragged {
			return nil, fmt.Errorf("sequence %q has uneven line lengths at line %d; "+
				"a .fai cannot describe that", cur.Name, lineNo)
		}
		if cur.LineBases == 0 {
			cur.LineBases, cur.LineWidth = bases, width
		} else if bases != cur.LineBases {
			if bases > cur.LineBases {
				return nil, fmt.Errorf("sequence %q has a long line at line %d; "+
					"a .fai cannot describe that", cur.Name, lineNo)
			}
			ragged = true
		}
		cur.Length += bases
		pos += width
		if err != nil {
			break
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out, nil
}

func writeFAI(path string, entries []Entry) error {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%d\t%d\t%d\t%d\n",
			e.Name, e.Length, e.Offset, e.LineBases, e.LineWidth)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writeGZI writes the BGZF block index: a count, then one (compressed,
// uncompressed) pair per block after the first, all little-endian uint64.
func writeGZI(path string, blocks []block) error {
	buf := make([]byte, 8+16*len(blocks))
	binary.LittleEndian.PutUint64(buf[:8], uint64(len(blocks)))
	for i, b := range blocks {
		off := 8 + 16*i
		binary.LittleEndian.PutUint64(buf[off:off+8], uint64(b.compressed))
		binary.LittleEndian.PutUint64(buf[off+8:off+16], uint64(b.uncompressed))
	}
	return os.WriteFile(path, buf, 0o644)
}
