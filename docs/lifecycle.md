# Source & tool lifecycle

There are two moments in a source's life: **acquisition** (`varhub download`, once) and
**annotation** (`varhub annotate`, per query). What happens at each depends on the source
`type`.

| source | `varhub download` | `varhub annotate` |
| --- | --- | --- |
| data (plain) | download file(s), ensure a tabix index | tabix lookup by coordinate |
| data (build recipe) | run the recipe once → cache + index the output | tabix lookup by coordinate |
| builtin | nothing to download | computed from the record (variant-only run on any path; the rest `-o` VCF only) |
| tool | acquire the container image + run one-time `setup` | run `steps` per novel locus (cached) |

`varhub download` handles all of these in one pass over the snapshot's sources
(`--source <name>` restricts to one; `--force` re-does work; `-j N` fetches N files at
once; `--keep-temp` keeps the per-source scratch dirs — the build workdir and tool
setup workdir — for debugging a recipe/setup pipeline). Builtins are skipped.

---

## Data source acquisition

A plain data source is downloaded from `url` (or used directly from `localpath`) and a
tabix index is ensured: a prebuilt `url_index` is downloaded if given, otherwise varhub
looks for one alongside the data, and builds one locally only as a last resort. Files are
cached under `cache_dir` keyed by `name/version`, so two snapshots referencing the same
source reuse one download. Checksums, when present, are verified while streaming (a
mismatch fails and leaves no partial file).

### Provisioning into an object store

`cache_dir` may be an `s3://bucket/prefix` locator instead of a directory, and
`varhub download --to s3://bucket/prefix` overrides it for one run. The same resolution
that decides a local cache path decides the object key, so a source cached at
`<cache>/clinvar/2026-01/clinvar.vcf.gz` locally lands at exactly that key remotely.

Provisioning still needs local scratch, because a checksum cannot be verified and a tabix
index cannot be built against a bucket. Each file is staged locally, verified, indexed,
opened to prove the pair works, and only then uploaded — so the scratch requirement is the
**largest single file**, not the whole snapshot. Nothing that failed verification is ever
uploaded.

Details worth knowing:

- **Uploads are multipart**, so sources in the tens of gigabytes work. A failed upload is
  aborted, leaving no partial object a later run could mistake for a complete one.
- **The index is uploaded before the data.** A run interrupted between the two leaves a
  source that looks incomplete and gets redone, rather than one that looks complete but
  cannot be queried.
- **Re-running is a no-op.** A source counts as present only when its data *and* index are
  both there; `--force` re-downloads and re-uploads.
- **GTF sources publish only the converted form.** varhub bgzips and tabix-indexes the
  original and normally prunes it, so uploading the original first would just be paying to
  transfer something that gets deleted. `--keep-raw` uploads it as well.
- **Credentials** come from the standard AWS chain — environment, shared config, then an
  instance or container role — so a deployed pod can use an assumed role rather than static
  keys. `AWS_ENDPOINT_URL` points at a non-AWS, S3-compatible target and switches on
  path-style addressing.

### Annotating from an object store

`annotate` reads an `s3://` cache directly, with no local copy of the data. Each source is
opened where it sits and queried with **range requests**: the tabix index is fetched whole
(it is small), then only the blocks covering the queried regions are pulled. A narrow query
against a 2 MB indexed VCF transfers under 5% of it.

This applies to every indexed format — VCF, BED, tab, bigWig, bigBed, and the converted GTF
gene model. Two consequences worth knowing:

- **A GTF must be indexed.** Locally, a GTF with no index falls back to loading the whole
  file into memory; remotely that would mean streaming the entire object on every run, so it
  is an error instead. `varhub download` builds the index.
- **Credentials are needed at annotate time**, not just at download time, and come from the
  same standard AWS chain.

### Build sources

Some data needs preprocessing before tabix can use it (e.g. REVEL: many CSV zips →
convert → merge → index). A `[sources.build]` recipe travels with the source, so it stays
self-contained and registry-shareable:

```toml
[[sources]]
name     = "revel"
version  = "1.3"
format   = "tab"
ref_col  = 3
alt_col  = 4
url      = "https://sites.google.com/site/revelgenomics/"   # provenance only
requires = ["unzip", "python3"]                             # host deps, preflighted

  [sources.build]
  output = "merged.revel.hg38.txt.gz"
  inputs = ["https://…/revel_chrom_21_*.csv.zip", "…"]       # downloaded into {inputs}/
  assets = ["convert_csv_to_tab.py"]                         # a URL, or a co-located file
  run = [                                                    # shell steps; the last must write {output}
    "for z in {inputs}/*.zip; do unzip -o $z -d {workdir}; done",
    "python3 {workdir}/convert_csv_to_tab.py {workdir}/*.csv | varhub bgzip -o {output} -s 1 -b 2 -e 2 -S 1",
  ]
```

`varhub download` runs the recipe once (cached; `--force` rebuilds). A build source is
**input-independent** — it runs on static `inputs`, never on your query variants — which
is exactly what distinguishes it from a tool. Step placeholders: `{workdir}` `{inputs}`
`{output}` `{threads}`. **Assets** are URLs, or paths relative to the source's version
directory (a relative asset ships next to the source in the registry).

---

## Tool acquisition (image + one-time setup)

`varhub download` acquires a tool source's container image and runs its one-time setup:

1. **Image** — `image` is a registry ref (`docker://`, `oras://`, `shub://`) that is
   pulled, or a `.sif` URL that is downloaded. Cached under
   `cache/tools/<name>/<version>/`; skipped if present unless `--force`.
2. **Setup** — the `[[sources.setup]]` steps run **once** to install the tool's data into
   its persistent data dir (`{datadir}`). Setup is sentinel-gated (a
   `.varhub-setup-done` marker), so it runs on the first download and is skipped
   thereafter unless `--force`.

```toml
  [[sources.setup]]                            # runs once, at download
  name      = "install"
  container = true
  run = "INSTALL.pl -c {datadir} -r {datadir}/plugins -a cfp -g CSN -s homo_sapiens -y GRCh38"

  [[sources.setup]]                            # a host step can e.g. curl a prebuilt DB
  name      = "polyphen-sift-db"
  container = false
  run = "test -f {datadir}/PolyPhen_SIFT.db || curl -L --fail -o {datadir}/PolyPhen_SIFT.db https://…/…db"
```

## Tool per-run steps (pre/post-processing)

At `annotate` time, the `[[sources.steps]]` run in order over the novel query variants.
Steps can run inside the container (`container = true`) or on the host
(`container = false`); the workdir is shared between them, so a containerized step can
write an intermediate file that a host step post-processes.

```toml
  [[sources.steps]]                            # 1) VEP in the container → intermediate VCF
  name      = "vep"
  container = true
  run = "vep -i {input} -o {workdir}/vep.vcf --vcf --everything --cache --dir_cache {datadir} --fasta {ref} --fork {threads}"

  [[sources.steps]]                            # 2) host post-process → the final bgzipped output
  name      = "postprocess"
  container = false
  run = "python3 {workdir}/expand_vep_vcf.py < {workdir}/vep.vcf | python3 {workdir}/worst.py | varhub bgzip > {output}"
```

The last step must write `{output}` (default `<name>.<format>.gz`), which varhub then reads
back like a data source of the tool's `format`.

Each step runs as a local subprocess (container steps are wrapped with the tool's container
engine). A tool's **`threads`** (default 1) fills the `{threads}` placeholder — e.g. a VEP
step's `--fork {threads}`.

### Container mount contract

Inside a `container = true` step, placeholders resolve to **fixed in-container
mountpoints**, *independent of the host layout* — varhub binds the matching host dirs to
them. This is what makes a tool source portable enough to share via a registry: the author
never has to know where the end user's cache lives.

| placeholder | in-container value | host dir bound there |
| --- | --- | --- |
| `{datadir}` | `/varhub/data` | the tool's persistent data dir |
| `{workdir}` | `/varhub/work` | the per-run scratch dir |
| `{ref}` | `/varhub/ref/<file>` | the reference FASTA's dir |
| `{input}` | `/varhub/in/<file>` | the input file's dir |
| `{output}` | `/varhub/work/<file>` | (written under the workdir) |
| `{threads}` | thread count | — |

A `container = false` step keeps **real host paths** for the same placeholders — handy for
post-processing scripts that run outside the image.

### Helper scripts → `assets`

A tool source lists any co-located helper files it needs in `assets = [...]` (filenames
next to the source's `.toml`, or URLs). varhub stages each into the step workdir before
every run, so a step references one as `{workdir}/<name>` — no `PATH` or shebang reliance,
and it works in host *and* container steps (the workdir is bound at `/varhub/work`).
Declaring them also lets the registry bundle the scripts with the source.

### Required software (`requires`)

A tool or build source lists the host executables it needs (`requires = ["python3",
"unzip"]`). `varhub download` and `varhub annotate` check them with one lookup up front and
fail fast with a clear message if any is missing, instead of erroring partway through. A
tool's container engine (apptainer/singularity) is checked automatically, so it needn't be
listed.

---

## Built-in `bgzip` / `tabix`

So that recipes and tool scripts don't depend on external `bgzip`/`tabix` being installed,
varhub ships hidden `bgzip` and `tabix` subcommands backed by its `hts` library:

- `varhub bgzip [-o FILE] [file]` — BGZF-compress a file or stdin. Adding a tabix
  preset/columns (`-p vcf|bed|gff`, or `-s`/`-b`/`-e`/`-S`/`-c`/`-0`) **with `-o`** also
  writes the index in one step.
- `varhub tabix [preset|cols] FILE` — write a tabix index for an existing `.gz`.

Call them as `varhub bgzip` / `varhub tabix` from your `run` steps (varhub is on `PATH` when
your recipe runs).

Next: **[Input & output formats](io-formats.md)**.

## GTF conversion and the original download

A GTF that does not ship a tabix index cannot be queried by position, so
`download` converts it: the file is streamed through a tabix writer in GFF mode
(comment lines dropped) and written as **BGZF** — gzip in independent blocks, so
a query can seek to one block instead of decompressing the whole file. The result
is *larger* than a plain-gzip original, because resetting the compressor every
block costs ratio. That is the price of random access.

The original is removed once the conversion succeeds, since nothing reads it
again except a re-index and `--force` re-downloads anyway. Pass `--keep-raw` to
keep it, at roughly double the space.

Pruning is refused unless the converted file *and* its index are both present, and
never touches a source that shipped pre-indexed — there the original **is** the
queried file.
