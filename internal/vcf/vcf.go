// Package vcf reads sites-only loci from a VCF. Per the privacy boundary it
// consumes only CHROM/POS/REF/ALT and silently drops GT/FORMAT and INFO —
// nothing sample-derived is read or retained.
package vcf

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// ReadFile parses loci from a VCF file (plain or gzipped). Multi-allelic ALTs
// are split into one locus per allele (a minimal normalization step).
func ReadFile(path string) ([]model.Locus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open vcf %s: %w", path, err)
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gunzip %s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	return Read(r)
}

// WriteLoci writes loci as a minimal sites-only VCF (one record per locus),
// sorted by chrom then position so the file is tabix-friendly. Used to materialize
// requested loci as input for an external tool (e.g. VEP) on the cache/locus path.
func WriteLoci(path string, loci []model.Locus) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create vcf %s: %w", path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	sorted := append([]model.Locus(nil), loci...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Chrom != sorted[j].Chrom {
			return sorted[i].Chrom < sorted[j].Chrom
		}
		return sorted[i].Pos < sorted[j].Pos
	})

	fmt.Fprintln(w, "##fileformat=VCFv4.2")
	fmt.Fprintln(w, "#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, l := range sorted {
		if _, err := fmt.Fprintf(w, "%s\t%d\t.\t%s\t%s\t.\t.\t.\n", l.Chrom, l.Pos, l.Ref, l.Alt); err != nil {
			return err
		}
	}
	return w.Flush()
}

// ParseLocus parses a single "chrom:pos:ref:alt" locus (pos is 1-based). REF/ALT
// are upper-cased to match VCF ingestion.
func ParseLocus(s string) (model.Locus, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return model.Locus{}, fmt.Errorf("bad locus %q: want chrom:pos:ref:alt", s)
	}
	pos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return model.Locus{}, fmt.Errorf("bad locus %q: pos %w", s, err)
	}
	return model.Locus{
		Chrom: parts[0], Pos: pos,
		Ref: strings.ToUpper(parts[2]), Alt: strings.ToUpper(parts[3]),
	}, nil
}

// Read parses loci from a VCF stream.
func Read(r io.Reader) ([]model.Locus, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	var out []model.Locus
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue // header / meta lines
		}
		fields := strings.Split(text, "\t")
		if len(fields) < 5 {
			return nil, fmt.Errorf("vcf line %d: expected at least 5 columns, got %d", line, len(fields))
		}
		// Columns: CHROM POS ID REF ALT [...]. Everything past ALT (QUAL,
		// FILTER, INFO, FORMAT, samples) is intentionally ignored.
		chrom := fields[0]
		pos, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("vcf line %d: bad POS %q: %w", line, fields[1], err)
		}
		ref := strings.ToUpper(fields[3])
		for _, alt := range strings.Split(fields[4], ",") {
			alt = strings.ToUpper(strings.TrimSpace(alt))
			if alt == "" || alt == "." {
				continue
			}
			out = append(out, model.Locus{Chrom: chrom, Pos: pos, Ref: ref, Alt: alt})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read vcf: %w", err)
	}
	return out, nil
}
