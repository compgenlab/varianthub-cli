# Overview & config model

cganno is config-driven: you declare **sources** and the **annotations** they expose in
TOML, group them into a **snapshot**, then annotate against that snapshot. Nothing about
adding a new annotation requires code.

## CGANNO_HOME and config.toml

`CGANNO_HOME` is the base directory holding `config.toml`. It is chosen as, in order:
the `-home DIR` flag, else `$CGANNO_HOME`, else the current directory. Config values may
reference `$CGANNO_HOME` (e.g. `data_dir = "$CGANNO_HOME/data"`), so data, cache, and the
annotations tree all live under the home no matter where you run cganno from.

```toml
# config.toml
data_dir         = "$CGANNO_HOME/data"        # base for relative `localpath` files
cache_dir        = "$CGANNO_HOME/data/cache"  # downloaded sources + tool images/data
default_snapshot = "2026-07"
annotations_dir  = "./annotations"           # root for sources/ and snapshots/

[database]                                   # OPTIONAL — omit to skip the cache entirely
backend = "sqlite"
path    = "$CGANNO_HOME/cganno.db"

[references.GRCh38]                           # reference FASTAs, keyed by assembly
fasta = "/data/ref/GRCh38.fa"
```

Run `cganno init` to generate a starter `config.toml` (it prompts for the annotations
directory and the starter snapshot name) plus a `sources/` tree and a snapshot manifest.

## The annotations tree

Under `annotations_dir`, cganno keeps reusable **sources** and the **snapshot manifests**
that reference them. Versioned directories mirror the registry 1:1, so multiple versions
coexist and local↔registry paths match:

```
<annotations_dir>/
  sources/<name>/<version>/<name>-<version>.toml   (+ co-located build/tool assets)
  snapshots/<name>.toml                            (manifest: refs + settings)
```

A tool is just a source (`type = "tool"`), so it lives under `sources/` too — there is
no separate `tools/` directory. (The *cache* keeps a tool's container image and installed
data under `cache/tools/<name>/<version>/`; that's an internal cache, unrelated to the
annotations tree.)

## Snapshot

A **snapshot** is the unit `cganno annotate` runs against, and the version stamped on
results. It is a manifest file that references sources by `name:version` and carries the
snapshot-scoped settings:

```toml
# snapshots/2026-07.toml
title               = "July 2026 Clinical Panel"     # human-readable name (optional; falls back to the slug)
description         = "GRCh38 clinical annotation set"
assembly            = "GRCh38"                       # scopes the cache + verifies sources
sources             = ["builtins:1", "clinvar:2026-01", "gnomad:4.1", "vep:113"]
default_annotations = ["clinvar_sig", "gnomad_af", "vep_consequence"]
```

The snapshot's short filename (`2026-07`) is its **slug** — the id used in refs and the cache
stamp; `title` is the optional human-readable name shown in `snapshot list`, `registry list`,
and the server UI. Sources have the same `title` (slug = `name`).

- **Apply order = list order.** A bare `name` (no `:version`) resolves to the sole
  version on disk.
- **`assembly`** is per-snapshot (a snapshot is inherently assembly-specific). It is
  verified against each source's declared `assembly` at load time, and it scopes the
  cache so GRCh37 and GRCh38 never collide. The reference FASTA is looked up from the
  config's `[references.<assembly>]` — it is not pinned in the manifest.
- **`default_annotations`** are the annotations applied when `annotate` runs with no
  `--all`/`-a`. Managed here (not on the source), so one source can be default in one
  snapshot and opt-in in another.

Manage everything interactively with **`cganno configure`** — a TUI whose home menu has
three areas: **Config settings** (edit `config.toml`), **Sources** (browse/add/edit the
whole local source library), and **Snapshots** (pick a snapshot, then checkbox-select its
member sources and default annotations). The same tasks are scriptable via
`cganno snapshot add|list`, `cganno source add`, and `cganno annotation add`.

## Annotation

An **annotation** is a declared output field nested under a source. It has a `name`
(the new INFO tag added / the column emitted) filled from the source's `field` (an INFO
id, a BED/TSV column, `@ID`, or a GTF field). `field` defaults to `name`.

`type` controls interpretation and storage (default `categorical`):

| type | meaning |
| --- | --- |
| `categorical` | a string from a limited set (`values = [...]` optionally documents the enum) |
| `text` | a free-form string (descriptions, HGVS, gene lists) |
| `numeric` | a float, stored numerically; a non-numeric value is dropped for that locus |
| `flag` | presence only, no value; `field` is ignored |

Add annotations with `cganno annotation add --source <name:version> --name … --field …`,
or in the TUI. `cganno annotation list <snapshot>` shows every annotation with the default
set marked `*`.

## The cache

The annotation cache is an **optional** SQLite table that memoizes computed values, keyed
by **assembly + locus + source `name:version`**. Omit the `[database]` block and cganno
computes without persisting (no `cganno.db` is written); add it to memoize so repeat
lookups are instant and expensive tools run only on novel loci.

The cache keys on the *locus*, not the annotation set: once a locus is cached it is
treated as fully annotated, so values added later won't appear until you query a fresh
locus or delete the cache DB. It is a rebuildable memo — deleting it is always safe.

See **[Source types](sources.md)** next.
