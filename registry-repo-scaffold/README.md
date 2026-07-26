# varianthub-public-data-registry

A catalog of **annotation source/snapshot configs** for [VariantHub](https://github.com/compgenlab/varianthub-cli).
It holds *configurations, not data* — `varhub download` fetches the actual files.

## Layout

```
registry.toml                              catalog: [[sources]]/[[snapshots]] entries (each has a `file`)
sources/<name>/<ver>/<name>-<ver>.toml     one source's config snippet (a source + its annotations)
snapshots/<name>.toml                      a full snapshot bundle (references sources by name:version)
```

`registry.toml` is served over HTTPS (GitHub raw, Pages, S3 — anywhere). varhub
points at its URL; entry `file` paths resolve relative to it — so the on-disk name is
free (the layout above is just the convention). Versions are tags: `add-source
clinvar:2026-01`, or `clinvar` / `clinvar:latest` for the entry marked `latest = true`
(versions aren't auto-sorted, so the publisher declares latest).

## Consuming it

```sh
varhub registry list                                          # uses this registry by default
varhub registry add-source clinvar:2026-01 --snapshot 2026-07 # add one source to a snapshot
varhub registry add-source vep:113 --snapshot 2026-07         # a tool source works the same way
varhub registry pull-snapshot 2026-07                         # or pull a whole snapshot (+ its sources)
```

## Contributing a source or tool

Run `varhub registry submit <name[:version]>` (needs a `public_repo`
`GITHUB_TOKEN`), or open an issue with the **`source-submission`** label and the
config in a ` ```toml ` block. Submissions are always `[[sources]]` fragments — a tool is
just a `type = "tool"` source. A submitted data source **must declare a `checksum`**
(`md5`/`sha1`/`sha256`) for its data file. The
[issue-to-pr workflow](.github/workflows/issue-to-pr.yml) parses the `[[sources]]`
block, writes `sources/<name>/<version>/<name>-<version>.toml`,
updates the `registry.toml` entry (marking the new version `latest`), and opens a PR
closing the issue — a maintainer reviews and merges.

**Helper scripts** declared via `[[sources.build]].assets` (build sources) or a tool
source's top-level `assets` ride in
the issue body as a base64 `tar.gz` block (` ```assets.tar.gz.base64 `); the workflow
unpacks them into **individual files** next to the fragment, so the PR carries real,
reviewable scripts. (GitHub's API can't attach files, so the tarball is embedded in the
body — keep assets to text scripts; large/binary data belongs in a `url`/`setup`
download.)

## Setup (one-time, for maintainers)

GitHub blocks the default `GITHUB_TOKEN` from creating pull requests unless *"Allow
GitHub Actions to create and approve pull requests"* is enabled — and an org can
forbid it org-wide, in which case the repo setting can't override it (the workflow
fails with `GitHub Actions is not permitted to create or approve pull requests`: the
branch is pushed but no PR is opened).

To avoid touching org-wide policy, the workflow opens the PR with a **fine-grained
PAT** instead of `GITHUB_TOKEN` (a user token isn't subject to that restriction).
Configure it once:

1. Create a **fine-grained personal access token** scoped to this repo with
   permissions *Pull requests: read & write* and *Contents: read & write*.
2. Add it as a repo secret named **`REGISTRY_PR_SECRET`** (Settings → Secrets and
   variables → Actions → New repository secret).

The workflow passes `token: ${{ secrets.REGISTRY_PR_SECRET }}` to
`create-pull-request`. (Alternatively, if you control the org and are fine loosening
it, enable the org-level "Allow GitHub Actions to create and approve pull requests"
and drop the `token:` line.)
