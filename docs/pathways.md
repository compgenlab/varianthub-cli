# Annotation pathways

`varhub annotate` has **two internal pathways**. Which one runs is decided by the output
`--format`, and they differ in what they can compute, whether samples are visible, how tools run,
and whether results are cached. Understanding the split explains why a few annotations are
"VCF-only" and why the cache behaves the way it does.

| | **VCF pipeline** | **Locus / cache path** |
| --- | --- | --- |
| selected by | `--format vcf` (with or without `-o`) | `--format tab` (default) `\| json \| text` |
| input | a VCF (samples preserved); bare loci are synthesized into a sites-only VCF | loci on the command line, or a VCF read as bare loci |
| how sources are read | streamed through the hts pipeline, each source looked up per record | each locus looked up by coordinate against each source's tabix index (the *overlay*) |
| samples / `FORMAT` | **yes** — preserved and available to builtins | **no** — sample-blind |
| builtins that run | **all** (incl. `vardist` + sample-derived) | **variant-only** (`auto_id`, `indel`, `tstv`, `tags`) |
| external tools | run in **bulk** over the whole input | run **per novel locus**, memoized |
| caching | none (recomputed each run) | optional SQLite memo (keyed by assembly + locus + source `name:version`) |
| parallelism | `-t N` (see [Parallel & distributed](parallel.md)) | instant on cache hits; tools skip already-seen loci |
| output | a fully-annotated VCF | a table / JSON / plain values |

Both pathways apply the **same selected annotation set** — chosen by `--all`, `-a name[,name…]`, or
(with neither) the snapshot's `default_annotations` — and both draw from the **same sources** in the
snapshot. They differ only in *how* the values are produced.

## The VCF pipeline (`--format vcf`)

Reads the input VCF (or synthesizes a sites-only VCF from bare loci), builds one streaming pipeline
over **every selected source and builtin**, and writes a VCF with the annotations added as `INFO`
(and `FORMAT`, for sample-derived builtins) tags. Because it sees the real records and their
samples, this is the **only** path that can run:

- `vardist` — needs the neighboring variants in the stream (a look-ahead), so it requires the whole
  per-contig order.
- the sample-derived builtins — `dosage`/`vaf`/`minor_strand`/`fisher_sb`/`copy_logratio` read
  per-sample `FORMAT` fields (GT/SAC/AD).

External tools run **in bulk**: each tool executes once over the whole input, producing an indexed
output file that the pipeline then reads like a data source. (The per-locus tool cache is skipped
here — for a whole VCF, round-tripping every line through SQLite would be pure overhead.) This path
is the one you parallelize for large inputs — see **[Parallel & distributed annotation](parallel.md)**.

## The locus / cache path (`--format tab|json|text`)

Answers one locus at a time. Each locus is looked up by genomic coordinate against every selected
source's tabix index and the matching values are collected (the *overlay*), then memoized in the
optional annotation cache. It is **sample-blind** — it works from `chrom/pos/ref/alt` only — so of
the builtins only the **variant-only** ones (`auto_id`, `indel`, `tstv`, `tags`) contribute a
column/key here; a builtin that doesn't apply to a locus (e.g. `tstv` on an indel) yields a
blank/`null`.

External tools on this path run **per locus and are cached**: a tool executes only on loci it hasn't
seen for this `name:version` + assembly, and prior results are served from the cache. This is what
makes an expensive tool like VEP practical for interactive, one-locus-at-a-time queries.

### The cache

The cache (the optional `[database]` block; see **[Overview](overview.md#the-cache)**) is a
rebuildable memo keyed by **assembly + locus + source `name:version`**. It keys on the *locus*, not
the annotation set: once a locus is cached it is treated as fully annotated, so an annotation you
add later won't appear for an already-cached locus until you query a fresh one or delete the cache.
Deleting `varhub.db` is always safe. Omit `[database]` entirely to compute without persisting.

## Choosing a pathway

- Annotating a **cohort VCF**, or you need samples / `vardist` / a fully-annotated VCF out →
  `--format vcf`. Parallelize it with `-t N`, or a job array (see [Parallel & distributed](parallel.md)).
- **Interactive lookups** of individual loci, or a tabular/JSON report, and you want repeat queries
  to be instant → the default `--format tab` (or `json`/`text`) with the cache enabled.

---

See also **[Source types](sources.md)** (what each source computes) and **[Source & tool
lifecycle](lifecycle.md)** (when tools acquire and run).
