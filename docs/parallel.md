# Parallel & distributed annotation

For `--format vcf`, VariantHub annotates a large VCF faster in two independent ways:

- **On one machine** — `-t N` fans the work out across annotation **sources**.
- **Across many variants / to beat a huge source** — split the input into batches
  **outside varhub** (with [cgkit](https://github.com/compgenlab/cgio-hts)) and run one
  independent `varhub annotate` job per batch, on a scheduler or a shell loop.

varhub deliberately does **not** split or concatenate VCFs itself: that is a job for the
caller (a scheduler + cgkit), which keeps each varhub invocation a simple, single-input
annotate.

> This applies to VCF output only (`varhub annotate --format vcf`, the streaming pipeline). The
> individual-locus path (`--format tab|json|text`) parallelizes differently — it is memoized in
> the cache and answers repeat loci instantly. See **[Annotation pathways](pathways.md)**.

## On one machine: `-t N` (source fan-out)

`-t N` (`--threads N`) uses up to `N` workers; `-t 0` uses all CPUs; `-t 1` (default) is a single
pass. With `-t N > 1`, varhub runs the **per-source fan-out**: one full pass per source over the
whole input, concurrently, then merges the parts positionally into the output.

- Every builtin is grouped into a single whole-file job, so stream-stateful builtins like
  `vardist` (which look at neighboring variants) see the file in order and work correctly.
- A multi-file source (per-chromosome, or a `Files` union) expands to **one job per file**, so it
  parallelizes file-by-file.
- `--keep-temp` retains the temp part files for debugging.

```sh
varhub annotate --all --format vcf -t 8 -o out.vcf.gz in.vcf.gz
```

The catch: fan-out is **capped at the number of source jobs**. A single large *monolithic* source
over many variants is one job on one core — the long pole that `-t N` can't divide. To parallelize
*within* one huge source, split the input (below).

## Across many variants / a huge source: split the input

Every source annotator does a **random-access lookup per variant** (tabix/bigWig/bigBed/GTF all
seek the source at each variant's position), so annotation cost scales with the **number of
variants**, and a few very large sources dominate the wall time — one big source over a whole
chromosome is a single long job that `-t N` cannot break up. Splitting the *variants* into batches
divides that work — including those expensive lookups — evenly across independent jobs.

varhub doesn't ship split/concat commands; the **scatter → annotate → gather** is done with
**[cgkit](https://github.com/compgenlab/cgio-hts)** (`cgkit vcf-split` / `cgkit vcf-concat`), which
does variant-count splitting and a coordinate-sorted, header-unioning concat:

```sh
cgkit vcf-split in.vcf.gz --out work/part --num 100000       # → work/part.1.vcf.gz, part.2.vcf.gz, …
# one independent job per batch i:
varhub annotate --all --format vcf work/part.$i.vcf.gz -o work/part.$i.ann.vcf.gz
cgkit vcf-concat --chunks work/part.1.ann.vcf.gz -o out.vcf.gz   # walk the numbered sequence, merge
```

Why this fits a scheduler:

- The full input is read **once**, at `vcf-split`. Each annotate task reads only its small batch, so
  every task has a **short, predictable wall time** — no single job blows out your time estimate.
- Tasks are fully independent (embarrassingly parallel), so the array scales **across nodes**, not
  just the cores on one machine.
- Each batch is a standalone VCF carrying the full header and samples, annotated exactly like the
  whole file.

`cgkit vcf-concat` merges by contig order + position (a coordinate merge, so it's order-independent
and re-sorts if needed) and unions the parts' INFO/FORMAT/FILTER/ALT header definitions. `--chunks`
takes the **first** file of a numbered sequence (`part.1.ann.vcf.gz`, `part.2.ann.vcf.gz`, …) and
reads them one at a time, so recombining thousands of batches stays under the open-file limit.
Trade-off: the batch files stage on disk (≈ the input size) between scatter and gather.

### Worked SLURM example

```sh
# 1. scatter — once, in a prep step:
cgkit vcf-split in.vcf.gz --out "$SCRATCH/chunks/part" --num 100000
N=$(ls "$SCRATCH"/chunks/part.*.vcf.gz | wc -l)

# 2. array — one task per batch (1-indexed to match part.1 … part.N):
sbatch --array=1-$N annotate_chunk.sbatch
```

`annotate_chunk.sbatch`:

```sh
#!/bin/bash
#SBATCH -c 4                                   # cores per batch (optional; see below)
set -euo pipefail
export VARHUB_HOME=/path/to/varhub
i="$SLURM_ARRAY_TASK_ID"
varhub annotate --all --format vcf -t 4 \
  "$SCRATCH/chunks/part.$i.vcf.gz" \
  -o "$SCRATCH/chunks/part.$i.ann.vcf.gz"
```

```sh
# 3. gather — after the array completes:
cgkit vcf-concat --chunks "$SCRATCH/chunks/part.1.ann.vcf.gz" -o out.vcf.gz
```

Keeping the numeric field in the annotated names (`part.$i.ann.vcf.gz`) lets `--chunks` walk the
sequence from `part.1.ann.vcf.gz`. The two models compose: give each batch task `-t N` to also fan
out across sources *within* its batch on its node.

### No scheduler? A plain shell loop

```sh
cgkit vcf-split in.vcf.gz --out work/part --num 100000
ls work/part.*.vcf.gz | xargs -P8 -I{} sh -c \
  'varhub annotate --all --format vcf "{}" -o "${1%.vcf.gz}.ann.vcf.gz"' _ {}
cgkit vcf-concat --chunks work/part.1.ann.vcf.gz -o out.vcf.gz
```

## The `vardist` caveat

Splitting the input cuts the stream mid-contig, so the `vardist` builtin — distance to the
*nearest neighboring variant* — cannot be computed correctly across a batch boundary. For a
`vardist` run, **don't split the input**: annotate the whole file on one machine, where `-t N`
(and `-t 1`) keep `vardist` on the full ordered stream. Every other builtin and every data source
is per-record and batches cleanly.

## Recombining a hand-distributed per-source run

If instead of splitting variants you distribute **one `varhub annotate -a <source>` pass per
source** across a cluster (each producing the same records with different INFO/FORMAT), recombine
the per-source parts with `varhub vcf-merge` — a *column* combine of parts holding the **same**
records in the same order:

```sh
varhub vcf-merge -o out.vcf.gz part.A.vcf.gz part.B.vcf.gz …
```

This is different from the batch gather above (`cgkit vcf-concat`, which recombines **disjoint**
records). Neither is a bcftools-style site merge.

## Choosing the batch size (`cgkit vcf-split --num`)

Smaller batches ⇒ more jobs ⇒ better load balancing, but more per-job overhead (each job re-opens
every source and re-runs any tool setup). Aim for **many more batches than workers/nodes** while
keeping each batch substantial. 100000 variants is a good start; tune down if one batch is a
straggler, up if the per-job source-open cost shows.

---

See also **[Annotation pathways](pathways.md)** (VCF pipeline vs. the locus/cache path) and
**[Input & output formats](io-formats.md)**.
