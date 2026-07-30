# VariantHub

**Fast, no-fuss variant annotation from the command line.**

VariantHub annotates VCF files (and bare loci) against a *versioned* bundle of reference
sources — a gene model, ClinVar significance, gnomAD allele frequencies, CADD/REVEL
scores, your own BED/VCF/TSV tracks, or external tools like VEP — and caches results
so repeat work is instant. Use it from the command line or run it as an asynchronous
HTTP service. It's a single static Go binary named `varhub`: no Perl, no cache-install
dance, no database server to stand up.

> Built-in annotations are emitted as `CG_*` INFO tags, following the convention of the
> underlying [hts library](https://github.com/compgenlab/cghts).

## Highlights

- **One binary.** Pure Go (`CGO_ENABLED=0`), cross-compiles to Linux/macOS,
  amd64/arm64. No interpreter or system libraries to manage.
- **Versioned bundles.** A *snapshot* pins a set of `name:version` sources + their
  annotation schema, so output is reproducible and the config is git-friendly.
- **Config-driven.** Sources and the fields they expose are declared in TOML — no code
  to add a new annotation. Sources can be static files, built-in annotators, or
  external tools (VEP/ANNOVAR).
- **Memoizing cache.** Annotated loci are memoized in SQLite and served instantly
  thereafter; an external tool's output can be reused across runs (`--tool-cache-dir`)
  instead of re-running it.
- **Scales to whole genomes.** Parallel VCF annotation (`-t`) fans out across annotation
  sources on one machine; to parallelize a few very large sources or scale across nodes, split
  the input into batches outside VariantHub and run one job per batch (`cgkit vcf-split` /
  `vcf-concat`, e.g. across a job array). BGZF output, and GTF gene models that are
  tabix-indexed and queried by position stay memory-bounded rather than loaded whole into RAM.
- **Web-ready.** [varianthub-web](https://github.com/compgenlab/varianthub-web) wraps this
  binary in a REST API and web UI, running annotation jobs by invoking it — so the CLI and
  the service produce byte-identical output.

## Install

Requires Go 1.25+.

```sh
go install github.com/compgenlab/varianthub-cli/cmd/varhub@latest
```

## Quick start

```sh
export VARHUB_HOME=~/varhub                 # base dir for config, data, cache, and the DB
varhub init                                # scaffold config.toml + a starter snapshot

# add a source, reference it from the snapshot, then fetch the data:
varhub source add --name gnomad --version 4.1 --url https://… --format vcf --snapshot 2026-07
varhub annotation add --source gnomad:4.1 --name gnomad_af --field AF --type numeric
varhub download -j 4

# provision into an object store instead of a local cache (annotation still reads
# locally — see docs/lifecycle.md):
varhub download --to s3://my-bucket/varhub

# annotate (default output is TSV; --format vcf|json|text, -o writes to a file):
varhub annotate chr1:115256529:T:C
varhub annotate --all --format vcf -o out.vcf in.vcf

# whole-genome: annotate sources in parallel and write bgzipped VCF (-v for progress):
varhub annotate --all --format vcf -t 8 -v -o out.vcf.gz in.vcf.gz
```

See the **[Quick start guide](docs/quickstart.md)** for a fuller walkthrough.

For an HTTP API and a web UI over the same engine, see
**[varianthub-web](https://github.com/compgenlab/varianthub-web)**.

## Documentation

Full documentation lives in **[`docs/`](docs/README.md)**:

- **[Getting started](docs/quickstart.md)** — install, initialize, add a source, annotate.
- **[Overview](docs/overview.md)** — the config model, snapshots, and the cache.
- **[Annotation pathways](docs/pathways.md)** — the VCF pipeline vs. the individual-locus/cache
  path: what each computes, samples, tools, caching.
- **[Source types](docs/sources.md)** — builtin / vcf / tabix / bed / gtf / tool.
- **[Source & tool lifecycle](docs/lifecycle.md)** — download, build recipes, tool
  image-acquire + setup, and per-run pre/post-processing steps.
- **[Input & output formats](docs/io-formats.md)** — how variants go in and how results
  come out.
- **[Parallel & distributed annotation](docs/parallel.md)** — `-t N` source fan-out on one
  machine, and a `cgkit vcf-split` → array → `cgkit vcf-concat` job array across a scheduler.
- **[Registry](docs/registry.md)** — pulling pre-made sources, and submitting your own.

## Status

Early but working: an interactive CLI on a SQLite backend. A Postgres backend is planned.
Cohort-style filtering ("which loci are
pathogenic *and* rare") is intentionally out of scope — VariantHub produces the annotations; a
consumer filters them.

## Development

```sh
make build      # bin/varhub
make test       # go test -race ./...
make vet
make cross      # release tarballs (linux,darwin × amd64,arm64)
```

Supported platforms: Linux and macOS on `amd64` and `arm64`. Because VariantHub is pure Go,
`make cross` static-cross-compiles all four from any host with no C toolchain.

## License

Not yet chosen. (TODO: add a `LICENSE` file before distributing.)
