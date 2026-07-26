# Example annotations tree (config-model v2)

This directory is a complete `annotations_dir`: reusable **sources** at the top level
and a **snapshot manifest** that references them. Point a config at it with
`annotations_dir = ".../examples"` (or copy the pieces under your own `$VARHUB_HOME`),
then explore with `varhub annotation list 2026-06`.

```
examples/
  sources/
    builtins/1/builtins-1.toml         # self-contained hts builtins (type = "builtin")
    clinvar/2026-01/clinvar-2026-01.toml   # vcf source (INFO / @ID / flag annotations)
    gnomad/4.1/gnomad-4.1.toml         # multi-file vcf source ({chrom} template)
    dbsnp/b156/dbsnp-b156.toml         # local source (localpath, never downloaded)
    revel/hg38/revel-hg38.toml         # tab source (pre-built)
    revel/1.3/revel-1.3.toml           # tab source built from a recipe ([sources.build])
      convert_csv_to_tab.py            #   co-located build asset
    vep/112/vep-112.toml               # type = "tool" source (containerized annotator)
      expand_vep_vcf.py                #   co-located helper scripts (assets)
      vep_vcf_worst_consequence.py
  snapshots/
    2026-06.toml                       # manifest: source refs + assembly + defaults
```

Key ideas the tree shows:

- **Everything is a source**, discriminated by `type`: a data file (default), a
  `type = "builtin"` bundle, or a `type = "tool"` external annotator (VEP). They all
  live under `sources/<name>/<version>/` and are referenced by `name:version` from a
  snapshot's single `sources` list. `revel` appears as two coexisting versions
  (`revel:hg38`, `revel:1.3`); the snapshot pins one.
- **The snapshot manifest owns snapshot-scoped config** — `assembly` and
  `default_annotations` (the reference FASTA is looked up from the config by
  `assembly`). Defaults live here, *not* on the source, so one source can be default
  in one snapshot and opt-in in another.
- **Assets are co-located** with the source version that uses them and named in
  `assets` (tool sources) or `build.assets` (build sources). The `.py` files here are
  documented placeholders — replace them with the real scripts before running.

The `dbsnp:b156` source is on disk but *not* in the snapshot — on-disk sources are a
library; a snapshot references the subset it wants.
