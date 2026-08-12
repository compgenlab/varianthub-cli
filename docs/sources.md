# Source types

Everything VariantHub annotates from is a **source**, identified `name:version` and living at
`sources/<name>/<version>/<name>-<version>.toml`. A source is discriminated by its
`type` field:

| `type` | what it is | data on disk? | runs when? |
| --- | --- | --- | --- |
| `""` (default) | a **data file** in `vcf` / `tab` / `bed` / `gtf` format | yes (downloaded/built) | queried by coordinate |
| `builtin` | a **built-in annotator** computed from the record | no | variant-only on any path; the rest `-o` VCF only |
| `tool` | an **external annotator** (VEP/ANNOVAR) run per query | no (generates output) | at `annotate` time |

The litmus test between a data source and a tool source: *does producing the annotation
require seeing the query variants?* If no, it can be precomputed into a static file → a
data source (this is why **CADD is a data source** — precomputed scores for all SNVs). If
yes, it must run on your variants → a **tool** (this is why **VEP is a tool**).

Every source's annotations nest under it as `[[sources.annotations]]`.

---

## Data sources (`vcf` / `tab` / `bed` / `gtf`)

A data source is a tabix-indexed reference file (or a set of files). At annotate time each
locus is looked up by genomic coordinate against the source's tabix index, then matched.

### Common fields

```toml
[[sources]]
name      = "clinvar"                          # short slug (the id, with version)
version   = "2026-01"
title     = "ClinVar variant significance"    # human-readable name (optional; falls back to name)
description = "Clinical significance from NCBI ClinVar"  # optional one-liner
assembly  = "GRCh38"          # verified against the snapshot's assembly
format    = "vcf"             # vcf | bed | tab | gtf
url       = "https://…/clinvar.vcf.gz"        # canonical location (provenance + registry)
url_index = "https://…/clinvar.vcf.gz.tbi"    # a prebuilt index to download (else guessed/built)
localpath = "/data/clinvar.vcf.gz"            # this machine's actual file (used as-is, never downloaded)
checksum       = "md5:https://…/clinvar.vcf.gz.md5"   # optional "<algo>:<hex-or-url>"
checksum_index = "md5:…"
```

- **`url` vs `localpath`:** `url`/`url_index` are the canonical reference kept for
  provenance and the registry; `localpath`/`localpath_index` are this machine's files —
  when `localpath` is set the file is used exactly and never downloaded. Environment
  variables (`${VAR}`, `$VARHUB_HOME`) are expanded in `localpath`. `registry submit`
  strips `localpath`.
- **`checksum`/`checksum_index`** are optional (`md5`|`sha1`|`sha256`); verified while
  downloading when present. The value may be a URL to a checksum file.
- **`stream = true`** reads the source from its `url` instead of caching a copy.
  For something like gnomAD — tens of gigabytes, queried at a handful of loci —
  downloading it to read a fraction of a percent is the wrong trade: the index is
  fetched whole and the data by range request. `varhub download` skips such a
  source entirely. It requires a `url`, and cannot combine with `localpath` or
  `build`, which both mean "there is a local file".
- **Data must be coordinate sorted** when varhub has to build the index itself (no
  `url_index`, and no `.tbi`/`.csi` published beside the data). Two rules: positions
  ascend within a contig — equal positions are fine, so a multiallelic site split across
  lines is not a problem — and each contig appears as one contiguous block, though the
  order of the contigs themselves does not matter. Unsorted input is refused rather than
  indexed, because an index built over unsorted data is structurally valid and simply
  returns incomplete results. This applies to whatever a `[[sources.build]]` recipe emits
  and to tool output (VEP with `--fork`, and ANNOVAR, do not preserve record order), so
  sort in the recipe's last step:

  ```sh
  (grep '^#' out.vcf; grep -v '^#' out.vcf | sort -k1,1 -k2,2n) \
      | varhub bgzip -p vcf -o {output}
  ```

### Multi-file data sources

One source can span several files, all queried and merged:

- **Per-chromosome** — a `{chrom}` template in `url`/`localpath` plus a `chroms` list
  (e.g. gnomAD, one file per chromosome). Each locus is routed to its chromosome's file.
- **Explicit union** — `[[sources.files]]`, an explicit list (each with its own
  `url`/`checksum`) for a small union split by something other than chromosome (e.g.
  coding + indels). Every file is queried; ref/alt picks the match.

### Format specifics

- **`vcf`** — copy an INFO field (`field = "CLNSIG"`), the record ID (`field = "@ID"`), or
  presence (`type = "flag"`). `match = "exact"` (REF+ALT, the default) or `"position"`;
  `unique = true` de-duplicates multiple matches.
- **`tab`** (tabix-indexed TSV — REVEL, CADD, …) — `ref_col`/`alt_col` (1-based) are
  **optional**: set both for allele-aware matching, omit for position-only. The
  annotation's `field` is the value column (a number, or a header column name).
- **`bed`** — interval overlap; `field = "name"` (col 4), `"score"` (col 5), or a column
  number.
- **`gtf`** — a GTF gene model. varhub bgzip-compresses and tabix-indexes it (GFF preset)
  once at `download` — cached under `cache_dir` and reused — then **queries it by position**,
  reconstructing only the overlapping gene(s) per variant (bounded memory, so multiple large
  GTFs no longer blow up whole-genome runs). If a bgzipped + tabix-indexed GTF (with a sibling
  `.tbi`/`.csi`) is supplied via `localpath`, it is used directly. Without any index the whole
  file is loaded into memory as a fallback (with a stderr warning). Its annotations select from
  a fixed vocabulary of derived fields via `field`: `GENE`, `GENEID`, `STRAND`, `BIOTYPE`,
  `REGION`, `CODING`, `NONCODING`. `gtf_tags = [...]` restricts to features carrying every
  listed tag (e.g. `"basic"` for the GENCODE basic set).
- **`bigwig`** — a UCSC bigWig (`.bw`): one numeric value per base. The annotation is that
  value at the variant position (`type = "numeric"`; no `field`). BBI files are
  **self-indexed** — downloaded whole and queried in place, with no tabix step. Only
  base-resolution data is read (display zoom summaries are ignored), so values are exact.
- **`bigbed`** — a UCSC bigBed (`.bb`): interval overlap like `bed`. `field = "name"`
  (col 4), `"score"` (col 5), or a 1-based column number. Also self-indexed.

**Per-alt bigWig sets.** Allele-specific scores (AlphaMissense, CADD, REVEL) are published
as four bigWigs — one per alternate base (`a/c/g/t.bw`). Declare them with an `{alt}`
template; varhub fetches all four and routes each variant to the file for its alt base:

```toml
[[sources]]
name    = "alphamissense"
version = "1"
format  = "bigwig"
url     = "https://hgdownload.soe.ucsc.edu/gbdb/hg38/alphaMissense/{alt}.bw"
# alts  = ["a","c","g","t"]   # the default
  [[sources.annotations]]
  name = "am_pathogenicity"
  type = "numeric"
```

An indel (multi-base alt) matches no per-alt file and gets no value.

**Chromosome naming is auto-converted:** varhub builds a converter from the source file's
own reference names, so input/source naming (Ensembl `1` / UCSC `chr1` / NCBI
`NC_000001.11`) is matched automatically.

Data sources that need preprocessing before they can be indexed use a **build recipe** —
see **[lifecycle](lifecycle.md#build-sources)**.

---

## Builtin sources (`type = "builtin"`)

A builtin is a self-contained "built-in tool call" — an annotator computed directly from
the record, with no data file. Builtins live in one `type = "builtin"` source whose nested
annotations name the builtin and give the value an output `name`:

```toml
[[sources]]
name = "builtins"
version = "1"
type = "builtin"
  [[sources.annotations]]
  builtin = "tstv"
  name    = "tstv"          # the name its value appears under (column / JSON key)
  [[sources.annotations]]
  builtin = "tags"
  name    = "PANEL"
  args    = "PANEL:v1"      # parameterized builtins carry their argument in `args`
```

In the VCF pipeline (`varhub annotate --format vcf`, or `-o out.vcf`) builtins emit their
`CG_*` INFO tags. The *variant-only* builtins (`auto_id`, `indel`, `tstv`, `tags`) also run
on the engine/overlay path used by `--format tab|json|text` — they compute from
chrom/pos/ref/alt alone, so they need no VCF or samples. There each contributes a column /
JSON key under its `name` (a value, or blank/`null` where it doesn't apply — e.g. `tstv`
on an indel). `vardist` and the sample-derived builtins are `-o` VCF only (see below).

| builtin | tag | needs samples? | notes |
| --- | --- | --- | --- |
| `auto_id` | (sets ID) | no | synthesize a variant ID: `1-115256529-T-C` — chrom-pos-ref-alt without the `chr`, the form gnomAD and the variant portals use |
| `indel` | | no | flag indels |
| `tstv` | `CG_TSTV` | no | transition/transversion |
| `vardist` | `CG_VARDIST` | no | distance to the nearest variant (streaming look-ahead) |
| `tags` | (constant) | no | a constant tag/flag; `args = "KEY:VALUE"` or `"FLAG"` |
| `dosage` | `CG_DS` | yes (GT) | allele dosage |
| `vaf` | `CG_VAF` | yes (SAC) | variant allele fraction |
| `minor_strand` | `CG_SBPCT` | yes | minor-strand percent |
| `fisher_sb` | `CG_FSB` | yes | Fisher strand bias |
| `copy_logratio` | `CG_CNLR` | yes (AD) | copy-number log-ratio; `args = "SOMATIC:GERMLINE[:st:gt]"` |

*Variant-only* builtins (`auto_id`/`indel`/`tstv`/`tags`) compute from chrom/pos/ref/alt
alone, so they run on any path — VCF *or* bare loci (`tab`/`json`/`text`). `vardist` needs
the neighboring variants in a stream (look-ahead), and the *sample-derived* ones read
per-sample `FORMAT` (GT/SAC/AD) and need a VCF with samples — so those are `-o` VCF only.

---

## Tool sources (`type = "tool"`)

A tool source is an external, often containerized annotator (VEP, ANNOVAR, a custom
script) that runs **per query**: varhub hands it the query variants, it produces an output
file, and that output is consumed exactly like a data source of its `format`.

```toml
[[sources]]
type     = "tool"
name     = "vep"
version  = "113"
image    = "docker://ensemblorg/ensembl-vep:release_113.3"  # docker://|oras://|shub:// ref, or a .sif URL
engine   = "apptainer"                         # or "singularity"
format   = "vcf"                               # how the OUTPUT is read (vcf | tab)
# input_format = "vcf"                          # how variants are handed IN (see io-formats)
output   = "vep.vcf.gz"
threads  = 4                                   # → {threads} (e.g. vep --fork 4); default 1
requires = ["python3"]                         # host executables that must be on PATH
assets   = ["expand_vep_vcf.py", "worst.py"]   # co-located helper scripts

  [[sources.setup]]  …    # one-time install (see lifecycle)
  [[sources.steps]]  …    # per-run steps (see lifecycle)

  [[sources.annotations]]
  name  = "vep_consequence"
  field = "Consequence"                        # an INFO id in the VEP output VCF
  type  = "categorical"
```

Because a tool is expensive to run, its output is **cached per locus** (keyed by
`name:version` + the snapshot's assembly), so on later runs it executes only on loci it
hasn't seen. Bump `version` whenever the image, setup, or steps change — that invalidates
the cache.

A tool source runs only when a **selected annotation** references it, so an unused tool
(e.g. VEP when you only asked for gnomAD) never runs.

For how a tool receives variants and how its output is read, see
**[Input & output formats](io-formats.md)**. For the run/setup mechanics, see
**[lifecycle](lifecycle.md)**.

## Gene-list sources (`type = "genelist"`)

A gene-list source **flags a variant when the gene overlapping it is in a named list**,
using a GTF gene model already in the snapshot to resolve the variant → gene. For example, a
"germline cancer genes" list flags any variant landing in `BRCA1`, `TP53`, ….

```toml
[[sources]]
type    = "genelist"
name    = "germline_cancer_genes"
version = "1"
gtf     = "gencode:48"            # a gtf source in this snapshot ("name" or "name:version")
genes   = ["BRCA1", "BRCA2", "TP53"]   # inline, and/or:
# genes_file = "germline_cancer_genes.txt"   # one symbol per line (# comments ok);
                                             # resolved next to this fragment
# gene_field = "gene_name"        # match gene_name (default) or gene_id

  [[sources.annotations]]
  name        = "germline_cancer_gene"   # type defaults to "flag"
  description = "Variant in a germline cancer gene"
```

The referenced GTF is queried once per variant (its bgzip+tabix index is built by `varhub
download`, same as any GTF source). The annotation is a **flag**: present when the variant's
gene is in the set (matched case-insensitively), absent otherwise. You can define several
gene-list sources over the same GTF (e.g. germline cancer, actionable, drug-target).

With `gene_field = "gene_id"`, the **version suffix Ensembl and GENCODE append is ignored on
both sides**: `ENSG00000141510` and `ENSG00000141510.17` are the same gene. The suffix counts
revisions of the gene's model rather than the gene, so a list pinned to one of them would
silently stop matching on the next GENCODE release. Write the ids either way.

### Listing a GTF's genes

```
varhub genes [--format tsv|json] <gtf-source[:version]>
```

Writes every gene in a GTF source — `gene_id` then `gene_name`, one per line, ids already
version-trimmed — so a list can be checked against a real gene model before it is written.
This streams the file rather than building the in-memory model an annotation run uses, so it
costs a linear read and a few megabytes rather than the whole gene structure.

## Commercial-use restrictions

Any source may set `non_commercial = true` to mark that its data/annotations carry
commercial-use restrictions (e.g. CADD). It is **informational only — nothing is blocked**; it
just makes the restriction visible.

```toml
[[sources]]
name           = "cadd"
version        = "1.7"
# …
non_commercial = true
```

The flag is surfaced by `varhub registry list` (a `[non-commercial]` marker), printed as a
notice by `varhub download`, and returned in the REST server's `GET /v1/annotations` discovery
(`"non_commercial": true`) so a public service can show it to users.
