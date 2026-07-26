# Registry

A **registry** is a catalog of pre-made **source configs** (not data) — a way to share and
discover sources instead of hand-writing each `.toml`. It is served as a plain static
`registry.toml` over HTTPS (GitHub raw, Pages, S3, any web host), so a registry has no
server to run.

Its on-disk layout mirrors the local annotations tree 1:1, so pulling a source drops it in
exactly where a local source lives:

```
registry.toml                                    # the catalog: [[snapshots]] + [[sources]]
sources/<name>/<version>/<name>-<version>.toml   # each source config (+ co-located assets)
snapshots/<name>.toml                            # snapshot manifests
```

Tool sources (`type = "tool"`) live under `[[sources]]` / `sources/…` like any other
source — there is no separate `[[tools]]` section.

## Configuring registries

When none is configured, varhub uses the built-in default
([`compgenlab/varianthub-public-data-registry`](https://github.com/compgenlab/varianthub-public-data-registry)).
Configure your own (one or several, searched in order) in `config.toml`:

```toml
registries = [
  "https://raw.githubusercontent.com/compgenlab/varianthub-public-data-registry/main/registry.toml",
  "https://example.org/my-lab-registry/registry.toml",
]
```

## Pulling from a registry

```sh
varhub registry list                              # catalog: snapshots + sources (all registries)
varhub registry pull-snapshot 2026-07             # write snapshots/2026-07.toml + all its sources
varhub registry add-source clinvar:2026-01 --snapshot 2026-07   # add one source, ref it from a snapshot
varhub registry add-source vep:113 --snapshot 2026-07           # a tool source works the same way
```

- **`pull-snapshot <name>`** fetches a snapshot manifest and every source it references
  (data + tool), reconstructing a full local tree.
- **`add-source <name[:version]>`** downloads one source config **and** its co-located
  helper assets (build scripts / tool post-processing scripts) into `sources/<name>/…`;
  with `--snapshot S` it also adds the ref to that snapshot's manifest. (`add-tool` is a
  kept alias for `add-source`.)
- **Versions are tags** (docker-style): `clinvar:2026-01` pins a version, while bare
  `clinvar` or `clinvar:latest` resolve to the entry the registry marks `latest = true`.
  varhub doesn't auto-sort versions (semver `1.3`, dbSNP `b157`, dates aren't comparable), so
  the publisher declares which is latest.

After pulling, run `varhub download` to fetch the actual data files / tool images (the
registry ships configs, not data). See **[lifecycle](lifecycle.md)**.

## Submitting a source to the public registry

You contribute a source (data or tool) by opening a GitHub issue containing its config; an
Action turns that issue into a pull request for a maintainer to review and merge.

1. **Build and test it locally.** Add the source to a snapshot, `varhub download` it, and
   confirm it annotates as expected.
2. **Submit:**

   ```sh
   varhub registry submit clinvar:2026-01               # a data source
   varhub registry submit vep:113                        # a tool source works too
   varhub registry submit clinvar:2026-01 --dry-run      # preview the issue title + body
   ```

   `submit` reads the named source fragment, strips machine-local fields (`localpath`), and
   opens a GitHub **issue** (the config in the body as a `toml` block, labeled
   `source-submission`). Any **co-located helper scripts** the source declares
   (`[sources.build].assets`, or a tool source's `assets`) are bundled into the issue as
   individual files, so a tool's post-processing scripts travel with it.
   - With `GITHUB_TOKEN` set, `submit` opens the issue directly.
   - Without a token, it writes a `<name>_<version>.issue.md` file for you to paste into a
     new issue manually.

3. **Reproducibility requirement.** A registry source must be reconstructable without your
   local files. `submit` rejects a source that isn't:
   - a data source needs a `url`, a `[sources.build]` recipe, or per-file `url`s (a
     `localpath`-only source can't be shared);
   - a **tool** source needs a shareable `image` — a `docker://`-style ref or a downloadable
     `http(s)` `.sif` URL (a local `.sif` path can't be shared).

4. **Review → merge.** The registry repo's Action (`issue-to-pr`) parses the `toml` block,
   writes `sources/<name>/<version>/<name>-<version>.toml` (+ unpacks the bundled assets),
   updates `registry.toml`, and opens a PR. A maintainer reviews and merges; when the PR
   merges the issue auto-closes.

Submissions target the canonical repo (`compgenlab/varianthub-public-data-registry`); the
scaffolding for running your own registry repo (the Action, issue template, and a starter
`registry.toml`) lives in `registry-repo-scaffold/`.

Next: **[REST API](rest-api.md)**.
