# VariantHub documentation

`varhub` annotates VCF files and bare loci against a *versioned* bundle of reference
sources, caching results so repeat work is instant. It's a single static Go binary —
no interpreter, no cache-install dance, no database server.

This directory documents how the system is put together and how to use it.

## Contents

- **[Getting started](quickstart.md)** — install, initialize, add a source, download, annotate.
- **[Overview](overview.md)** — the config model (config.toml, `annotations_dir`,
  snapshots, sources, annotations) and the annotation cache.
- **[Annotation pathways](pathways.md)** — the two paths `annotate` runs (the VCF streaming
  pipeline vs. the individual-locus/cache path): what each can compute, samples, tools, caching.
- **[Source types](sources.md)** — the one `source` concept and its `type`s:
  `builtin`, and data files in `vcf` / `tab` (tabix) / `bed` / `gtf` format, and
  `tool` (an external per-query annotator). What each does and how its annotations
  read values.
- **[Source & tool lifecycle](lifecycle.md)** — download / build / tool image-acquire
  + one-time setup, and per-run pre/post-processing steps (container vs host,
  placeholders, the `/varhub/*` mount contract). Includes the built-in
  `bgzip`/`tabix` helpers.
- **[Input & output formats](io-formats.md)** — how variants are handed *in* (loci or
  a VCF; a tool's `input_format`) and how results come *out* (`varhub annotate
  --format tab|vcf|json|text`; a tool's output `format`).
- **[Parallel & distributed annotation](parallel.md)** — annotating large VCFs in parallel:
  the `-t N` source fan-out, and a job-array scatter/gather (`cgkit vcf-split` →
  annotate array → `cgkit vcf-concat`) for a scheduler.
- **[Registry](registry.md)** — using a registry to pull pre-made sources/snapshots,
  and submitting a source (data or tool) to the public registry.
- **[REST API](rest-api.md)** — *placeholder* for the planned annotation endpoint.

## The 30-second model

- A **source** is an annotation provider identified `name:version`. It is one of:
  a **data file** (`vcf`/`tab`/`bed`/`gtf`), a **`builtin`** (computed from the
  record, no data file), or a **`tool`** (an external, often containerized annotator
  that runs per query). All three live under `annotations_dir/sources/<name>/<version>/`.
- A **snapshot** (`snapshots/<name>.toml`) is a manifest that lists the sources it
  includes (by `name:version`) plus its `assembly` and `default_annotations`. The
  snapshot name is the version stamped on every result.
- An **annotation** is a declared output field nested under a source; `varhub annotate`
  applies a selected set and prints/writes the values.

```sh
varhub init                                   # scaffold config + a starter snapshot
varhub source add --name gnomad --version 4.1 --url … --format vcf --snapshot 2026-07
varhub download                               # fetch + index the snapshot's sources
varhub annotate chr1:115256529:T:C            # → TSV of the default annotations
```

See the **[Quick start](quickstart.md)** for the full walkthrough.
