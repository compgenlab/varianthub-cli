# Migrating to the v2 config model

The v2 layout makes **sources and tools reusable, top-level units** and turns a
**snapshot into a manifest file** that references them. If you have an existing
install laid out the old way, this guide converts it.

## What changed

| v1 | v2 |
| --- | --- |
| `snapshots/<name>/` **directory** with one `.toml` per source/tool (filename order) | `snapshots/<name>.toml` **manifest** referencing sources by `name:version` |
| sources/tools nested under one snapshot dir | reusable `sources/<name>/<version>/<name>-<version>.toml` at the top level (tool fragments too, as `type = "tool"` sources) |
| `snapshots_dir` config key | `annotations_dir` config key (root for `sources/`, `snapshots/`) |
| separate `[[tools]]` fragments in `tools/<name>/<version>/` and a snapshot `tools = [...]` list | tools **are sources**: `[[sources]]` with `type = "tool"` under `sources/<name>/<version>/`; refs go in the snapshot's single `sources = [...]` list |
| `default = true` on a source annotation | `default_annotations = [...]` in the snapshot manifest |
| `assembly` / `reference` only global | `assembly` is per-snapshot only (the global `assembly` key is removed); the reference FASTA is keyed by assembly in the config (`[references.<assembly>]`) and looked up from the snapshot's assembly — no per-snapshot `reference` |
| `[database]` required (sqlite forced) | `[database]` optional — omit it to skip the cache |
| `@` version separator (`vep@113`) | `:` version separator (`vep:113`) |

The cache schema also changed (annotations are now assembly-scoped). The cache is a
rebuildable memo, so just delete `varhub.db` and let it repopulate.

## Steps

Assume an old install with `snapshots/2026-06/{01_builtins,02_clinvar,06_vep}.toml`.

1. **Update `config.toml`.** Rename `snapshots_dir` → `annotations_dir`. Point it at
   the root that will hold `sources/`, `snapshots/` — the default is
   `./annotations` (i.e. `$VARHUB_HOME/annotations`); use `.` to keep them directly
   under `$VARHUB_HOME`. The `[database]` block is now optional — keep it to keep the cache,
   or remove it to compute without persisting. The global `assembly` key is removed —
   drop it (it's harmlessly ignored if left) and set `assembly` in each snapshot
   manifest instead (step 5).

2. **Move each source fragment** to its own version dir, renaming the file to
   `<name>-<version>.toml`:

   ```
   snapshots/2026-06/02_clinvar.toml  →  sources/clinvar/2026-01/clinvar-2026-01.toml
   snapshots/2026-06/01_builtins.toml →  sources/builtins/1/builtins-1.toml
   ```

   Move a source's **build/helper assets** (the files named in `build.assets`) into the
   same version dir next to the `.toml`.

   The builtins source needs an explicit `name`/`version` in v2 (e.g. `name = "builtins"`,
   `version = "1"`) — add them if the old fragment omitted them.

3. **Move each tool fragment** the same way, but into `sources/<name>/<version>/` (there
   is no separate `tools/` dir anymore), and move its `assets` scripts alongside:

   ```
   snapshots/2026-06/06_vep.toml  →  sources/vep/112/vep-112.toml
   ```

   Then convert the fragment to a `type = "tool"` source: rename `[[tools]]` → `[[sources]]`
   and **add `type = "tool"`**, and rename `[[tools.setup]]` → `[[sources.setup]]`,
   `[[tools.steps]]` → `[[sources.steps]]`, `[[tools.annotations]]` → `[[sources.annotations]]`.

4. **Strip `default = true`** from every source/tool annotation — collect those
   annotation names into the snapshot manifest's `default_annotations` instead.

5. **Write the snapshot manifest** `snapshots/2026-06.toml` referencing what you moved,
   preserving the old filename order in the single `sources` list (tool refs like
   `vep:112` go in it too — there is no separate `tools` list), and setting the
   snapshot's `assembly`. Move the old global `[reference]` FASTA into a per-assembly
   `[references.<assembly>]` table in `config.toml` — the snapshot's reference is
   looked up from there by its `assembly`, so the manifest has no `reference` key:

   ```toml
   # snapshots/2026-06.toml
   description         = "GRCh38 clinical annotation set"
   assembly            = "GRCh38"
   sources             = ["builtins:1", "clinvar:2026-01", "vep:112"]  # vep:112 is a type="tool" source
   default_annotations = ["clinvar_sig", "vep_consequence"]
   ```

   ```toml
   # config.toml
   [references.GRCh38]
   fasta = "$VARHUB_HOME/ref/GRCh38.fa.gz"
   ```

6. **Delete the old cache** so it rebuilds under the new (assembly-scoped) schema:

   ```sh
   rm -f "$VARHUB_HOME/varhub.db"
   ```

7. **Verify.** `varhub annotation list 2026-06` should show every annotation with the
   right ones marked `*` (default); `varhub download` should fetch the same sources.

## Notes

- **Strict parsing.** Do the tools→sources conversion completely: a leftover `[[tools]]`
  fragment or a manifest `tools = [...]` key is a hard error (consistent with the
  project's clean-migration stance), not silently ignored.
- The registry now uses `[[sources]]` only (no `[[tools]]`); tool catalog entries live
  under `[[sources]]`, and `registry add-tool` is just an alias for `add-source`.
- `varhub annotate` now **defaults to TSV** output (it used to print a human report). Pass
  `--format text` for the old report, or `--format vcf|json` for the other formats.
- The registry uses the same v2 layout, so `varhub registry pull-snapshot <name>`
  reconstructs a full v2 tree directly — for public snapshots that's easier than
  migrating by hand.
- The TUI (`varhub edit`) writes v2 files, so you can also rebuild a snapshot
  interactively: add each source (tool sources included), then use the **members** (`m`) and **defaults**
  (`d`) checkbox editors on the snapshot.
- A one-shot `varhub migrate` helper may land later; for now the moves above are a
  mechanical `git mv` + a manifest write.
