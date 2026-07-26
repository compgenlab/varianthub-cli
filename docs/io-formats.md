# Input & output formats

There are two "input/output" boundaries in VariantHub, and it helps to keep them separate:

1. **The `varhub annotate` command** — what variants you give it, and how it renders the
   results to you.
2. **A tool source** — how varhub hands the query variants *into* an external tool
   (`input_format`), and how it reads the tool's produced file back (`format`).

---

## Annotate input

You annotate either **bare loci** or a **VCF file**:

```sh
varhub annotate chr1:100:A:G chr2:200:C:T      # one or more chrom:pos:ref:alt loci
varhub annotate variants.vcf                   # every site in a VCF (multi-allelic ALTs are split)
```

Both drive the same per-source lookup — a coordinate query into each source's tabix index,
then ref/alt matching. A single variant is just a one-element list.

Two internal paths back this:

- **Locus / cache path** (`--format tab|json|text`) — results come from the engine and are
  memoized in the cache. This path is **sample-blind**: it reads only chrom/pos/ref/alt, so
  the *variant-only* builtins (`auto_id`/`indel`/`tstv`/`tags`) run here, but builtins that
  need sample `FORMAT` fields (or, like `vardist`, the neighboring variants) do not.
- **Pipeline path** (`--format vcf`) — the input streams record-by-record through the hts
  annotation pipeline, preserving the input's header and samples. This is the path where
  **all** builtins apply (including the sample-derived ones). For bare loci, a sites-only VCF
  is synthesized first.

## Annotate output

```sh
varhub annotate [--all | -a name,…] [--format tab|vcf|json|text] [-o FILE] <vcf|locus…>
```

- **`--format`** selects the rendering; the default is **`tab`**.
- **`-o FILE`** writes the output to a file (stdout if omitted) — for *any* format.
- **`-v` / `--verbose`** prints progress to **stderr** (stdout still carries the results):
  which phase is running (external tool, pipeline build, single pass vs. a per-source fan-out),
  a running variant/record count, per-job completion during a parallel (`-t N`) run, and — on
  the individual-locus path — external-tool cache hits (how many loci are novel vs. cached).
  Off by default.
- **`--all`** applies every annotation; **`-a name[,name…]`** applies the named ones;
  with neither, the snapshot's `default_annotations` are applied.

| format | shape |
| --- | --- |
| **`tab`** (default) | a `#`-commented header, then one row per variant: `chrom pos ref alt` then a column per selected annotation (missing = empty) |
| **`json`** | an array of per-variant objects `{chrom,pos,ref,alt,annotations:{name:value,…}}` (numeric annotations stay JSON numbers) |
| **`text`** | a human-readable report (the pre-1.0 default) |
| **`vcf`** | a fully-annotated VCF via the streaming pipeline (samples preserved for a VCF input; a sites-only VCF is synthesized for bare loci) |

```sh
varhub annotate chr1:100:A:G                       # → TSV (default)
varhub annotate --format json chr1:100:A:G         # → JSON array
varhub annotate --format vcf -o out.vcf in.vcf     # → annotated VCF (samples preserved)
varhub annotate -a clinvar_sig -o hits.tsv in.vcf  # → TSV of one annotation, to a file
```

### External tools & the tool-output cache

How an external `type="tool"` source (VEP/ANNOVAR) runs depends on the **input**:

- **Bulk VCF** — `--format vcf`, `annotate file.vcf`, and REST VCF uploads run the tool **once
  over the whole input** and annotate directly from its (already-indexed) output. The per-locus
  tool cache is **not** used: for a whole VCF, where most variants are novel, the round-trip of
  writing every result into the cache and reading it back to rebuild an index is pure overhead.
- **Individual loci** — `annotate chrom:pos:ref:alt` and REST single-locus lookups **do** use the
  per-locus cache: a tool runs only on loci it hasn't seen, so repeat lookups skip the (often
  slow, containerized) tool entirely.

On the bulk-VCF path, **`--tool-cache-dir DIR`** turns DIR into a per-input tool-output cache:

- **Reuse (automatic):** before running a tool, if DIR holds a saved output whose recorded input
  matches the current one — same absolute path, size, and mtime — for this tool `name:version`
  and assembly, that output is reused and the tool is **not** run. (A cache hit needs neither the
  tool nor its container engine, so you can annotate from a prior VEP run on a host without it.)
- **Save (automatic):** on a miss, the tool runs and its output (`<name>-<version>.<timestamp>.<fmt>.gz`
  + `.tbi`/`.csi`), a **run manifest** (`.run.toml`, recording the input), and a drop-in
  `[[sources]]` stub (`.toml`) are written to DIR. Timestamps keep runs from colliding; files are
  never overwritten.

Regenerating the input bumps its mtime, so a stale output is never reused. You can also reference
a saved file directly as a normal static source (drop its `.toml` stub under
`annotations_dir/sources/`).

The `tab` output columns are fixed (`chrom pos ref alt` + one per annotation) and are
unrelated to a `tab` *source's* `ref_col`/`alt_col` (which describe how that source file is
*read*).

### Parallel VCF annotation (`-t`)

For `--format vcf`, **`-t N`** (`--threads N`) annotates a large VCF in parallel by fanning the
work out across annotation **sources** (one pass per source, merged positionally). To parallelize
*within* a single huge source — or to scale across nodes — split the input into batches outside
varhub and run one `varhub annotate` job per batch (scatter with `cgkit vcf-split`, gather with
`cgkit vcf-concat`). See **[Parallel & distributed annotation](parallel.md)**.

```sh
varhub annotate --all --format vcf -t 8 -o out.vcf.gz in.vcf.gz
```

The full story — strategy trade-offs, the `vardist` caveat, a worked SLURM array example, and the
`concat` vs `vcf-merge` distinction — is in **[Parallel & distributed annotation](parallel.md)**.

---

## Tool source I/O

A tool source is defined by how varhub writes the query variants for it and how it reads the
result back. The variants always reach the tool as a **file** — a single-variant query is
just a one-line/one-record file; on the cache path only the **novel** loci are written.

### Tool input — `input_format`

Controls how `{input}` is written for the tool:

- **`"vcf"`** (default) — varhub materializes a sites-only VCF (or, on the `--format vcf`
  path, passes the input VCF with its samples). Use this for VCF tools like VEP.
- **a per-variant line template** — varhub writes one line per variant. For a variant-list
  tool (e.g. ANNOVAR's avinput):

  ```toml
  input_format = "{chrom}\t{pos}\t{ref}\t{alt}"
  # or: input_format = "{chrom}_{pos}:{ref}>{alt}"
  ```

  Placeholders: `{chrom}` `{pos}` `{pos0}` (0-based) `{ref}` `{alt}` `{end}`
  (`pos + len(ref) - 1`). No header line is written. A template must contain at least
  `{chrom}` and `{pos}`.

### Tool output — `format`

The tool's produced file is read back *exactly like a static source of that format*:

- **`"vcf"`** — each annotation's `field` is an INFO id (or `@ID`, or `type = "flag"`).
- **`"tab"`** — chrom in column 1, pos in column 2; ref/alt via `ref_col`/`alt_col`
  (omit → position match); each annotation's `field` is a 1-based column number or a
  header column name. Header lines are `#`-prefixed.

The output must carry coordinates so varhub can key each record back to a locus — which is
why `vcf` and `tab` are the two supported tool output formats. (A bare `text`/`json`
stream isn't keyable without coordinates; JSON output support may come later when a concrete
tool needs it.)

Next: **[Registry](registry.md)**.
