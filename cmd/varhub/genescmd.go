package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/compgenlab/cghts/gtf"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/fetch"
)

// cmdGenes lists the genes a GTF source knows about.
//
// This exists for callers that need the *set* of genes rather than an
// annotation: building a gene list means checking pasted symbols against a real
// gene model, and the only honest way to do that is to ask the file. A caller
// that is not this program — the web service, whose worker holds the data volume
// — has no business parsing GTFs itself, so it shells out here the same way it
// already does for annotate, columns and download.
//
// Output is a gene per line, gene_id then gene_name, so the caller can match on
// either. Version suffixes are trimmed from the id (see gtf.TrimGeneIDVersion);
// the same normalization is applied when a genelist source matches, so what is
// listed here is exactly what a list built from it should contain.
func cmdGenes(ctx context.Context, cfgPath, snapshot string, args []string) error {
	fs := flag.NewFlagSet("genes", flag.ContinueOnError)
	format := fs.String("format", "tsv", "output format: tsv | json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: genes [--format tsv|json] <gtf-source[:version]>")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	snap, err := cfg.LoadSnapshot(snapshot)
	if err != nil {
		return err
	}
	src, err := gtfSourceByRef(snap, rest[0])
	if err != nil {
		return err
	}

	r, err := fetch.OpenGTFStream(ctx, cfg, *src)
	if err != nil {
		return err
	}
	defer r.Close()

	// Buffered: a GENCODE GTF is ~78k genes, and an unbuffered write per gene
	// turns a linear scan into 78k syscalls on the reader's side of the pipe.
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	if strings.EqualFold(*format, "json") {
		return scanGenesJSON(w, r)
	}
	return gtf.ScanGenes(r, func(g gtf.GeneRef) error {
		_, err := fmt.Fprintf(w, "%s\t%s\n", gtf.TrimGeneIDVersion(g.GeneID), g.GeneName)
		return err
	})
}

// scanGenesJSON writes one JSON object per line rather than one array.
//
// A whole-array encoding would make the caller hold every gene in memory before
// it could act on any of them, which defeats the point of streaming the file in
// the first place.
func scanGenesJSON(w *bufio.Writer, r interface{ Read([]byte) (int, error) }) error {
	enc := json.NewEncoder(w)
	return gtf.ScanGenes(r, func(g gtf.GeneRef) error {
		return enc.Encode(struct {
			GeneID   string `json:"gene_id"`
			GeneName string `json:"gene_name"`
		}{gtf.TrimGeneIDVersion(g.GeneID), g.GeneName})
	})
}

// gtfSourceByRef resolves a "name" or "name:version" reference to a GTF source in
// the snapshot.
//
// Refusing a non-GTF source by name is the whole of the error worth reporting:
// asking a VCF for its genes is a mistake in the caller, not an empty result.
func gtfSourceByRef(snap *config.Snapshot, ref string) (*config.Source, error) {
	name, version, _ := strings.Cut(ref, ":")

	var named []config.Source
	for _, s := range snap.Sources {
		if !strings.EqualFold(s.Name, name) {
			continue
		}
		if version != "" && s.Version != version {
			continue
		}
		named = append(named, s)
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("no source %q in snapshot %s", ref, snap.Name)
	}
	for i := range named {
		if named[i].IsGTFSource() {
			return &named[i], nil
		}
	}
	return nil, fmt.Errorf("source %s is not a GTF source (format = %q)", named[0].ID(), named[0].Format)
}
