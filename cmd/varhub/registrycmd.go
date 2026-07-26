package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/registry"
)

// cmdRegistry handles `registry list|pull-snapshot|add-source|add-tool|submit`.
func cmdRegistry(ctx context.Context, cfgPath string, args []string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: registry <list|pull-snapshot|add-source|add-tool|submit>")
	}
	locs := cfg.RegistryLocations()

	switch args[0] {
	case "list":
		for _, loc := range locs {
			m, err := registry.Fetch(ctx, loc)
			if err != nil {
				return fmt.Errorf("registry %s: %w", loc, err)
			}
			fmt.Printf("# %s\n", loc)
			fmt.Println("snapshots:")
			for _, e := range m.Snapshots {
				fmt.Printf("  %-16s %-28s %s\n", e.Name, e.DisplayTitle(), e.Description)
			}
			fmt.Println("sources:") // includes type="tool" sources
			for _, e := range m.Sources {
				desc := e.Description
				if e.NonCommercial {
					desc += "  [non-commercial]"
				}
				fmt.Printf("  %-16s %-10s %-28s %-10s %s\n", e.Name, e.Version, e.DisplayTitle(), e.Assembly, desc)
			}
		}
		return nil

	case "pull-snapshot":
		if len(args) != 2 {
			return fmt.Errorf("usage: registry pull-snapshot <name>")
		}
		name := args[1]
		e, loc, err := findSnapshot(ctx, locs, name)
		if err != nil {
			return err
		}
		raw, err := registry.FetchFile(ctx, loc, e.File)
		if err != nil {
			return err
		}
		var mc config.SnapshotConfig
		if _, err := toml.Decode(string(raw), &mc); err != nil {
			return fmt.Errorf("snapshot manifest %q: %w", name, err)
		}
		dest := cfg.SnapshotFile(name)
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("snapshot %q already exists locally (%s)", name, dest)
		}
		// Pull each referenced source (data or tool), then write the manifest.
		for _, ref := range mc.Sources {
			if _, _, err := pullItem(ctx, cfg, locs, ref); err != nil {
				return fmt.Errorf("source %q: %w", ref, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("pulled snapshot %q → %s (%d sources)\n", name, dest, len(mc.Sources))
		return nil

	case "add-source", "add-tool": // add-tool is a kept alias — tools are sources now
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		snap := fs.String("snapshot", "", "also add the ref to this snapshot manifest")
		if len(args) < 2 {
			return fmt.Errorf("usage: registry %s <name[:version|:latest]> [--snapshot S]", args[0])
		}
		ref := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		n, v, err := pullItem(ctx, cfg, locs, ref)
		if err != nil {
			return err
		}
		fmt.Printf("added source %s:%s\n", n, v)
		if *snap != "" {
			if err := addRefToSnapshot(cfg, *snap, n+":"+v); err != nil {
				return err
			}
			fmt.Printf("added %s:%s to snapshot %q\n", n, v, *snap)
		}
		return nil

	case "submit":
		return cmdRegistrySubmit(ctx, cfg, args[1:])

	default:
		return fmt.Errorf("unknown registry subcommand %q", args[0])
	}
}

// findSnapshot searches the locations for a snapshot (manifest) entry.
func findSnapshot(ctx context.Context, locs []string, name string) (*registry.Entry, string, error) {
	for _, loc := range locs {
		m, err := registry.Fetch(ctx, loc)
		if err != nil {
			return nil, "", fmt.Errorf("registry %s: %w", loc, err)
		}
		if e := m.Snapshot(name); e != nil {
			return e, loc, nil
		}
	}
	return nil, "", fmt.Errorf("snapshot %q not found in any registry", name)
}

// pullItem fetches a registry source (data or tool) by ref and writes it into the
// local sources/ tree (+ co-located assets), returning its name and version.
func pullItem(ctx context.Context, cfg *config.Config, locs []string, ref string) (string, string, error) {
	name, version, _ := strings.Cut(ref, ":")
	e, loc, err := findSource(ctx, locs, name, version)
	if err != nil {
		return "", "", err
	}
	frag, err := registry.FetchFragment(ctx, loc, e.File)
	if err != nil {
		return "", "", err
	}
	stripLocal(frag)
	file, dir := cfg.SourceFile(e.Name, e.Version), cfg.SourceDir(e.Name, e.Version)
	if err := config.WriteFragment(file, frag); err != nil {
		return "", "", err
	}
	if _, err := fetchFragmentAssets(ctx, loc, e.File, dir, frag); err != nil {
		return "", "", err
	}
	return e.Name, e.Version, nil
}

// findSource searches the locations for a source entry (first match wins).
func findSource(ctx context.Context, locs []string, name, version string) (*registry.Entry, string, error) {
	for _, loc := range locs {
		m, err := registry.Fetch(ctx, loc)
		if err != nil {
			return nil, "", fmt.Errorf("registry %s: %w", loc, err)
		}
		e, err := m.SourceE(name, version)
		if err != nil {
			return nil, "", err // e.g. ambiguous latest across versions
		}
		if e != nil {
			return e, loc, nil
		}
	}
	return nil, "", fmt.Errorf("source %q not found in any registry", name)
}

// cmdRegistrySubmit opens a GitHub issue proposing a local source or tool (+ its
// annotations, and any co-located helper scripts) for the canonical registry.
// Submission only targets that repo.
func cmdRegistrySubmit(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: registry submit <name[:version]> [--dry-run]")
	}
	ref := args[0]
	name, version, _ := strings.Cut(ref, ":")
	fs := flag.NewFlagSet("registry submit", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the issue title + body instead of submitting")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	fragPath, frag, ok, err := findLocalItem(cfg, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no local source %q found", ref)
	}
	fragDir := filepath.Dir(fragPath)

	// Resolve the named source (data or type="tool") and build its clean snippet, the
	// issue title, and the list of co-located helper files to bundle.
	src := frag.SourceByName(name)
	if src == nil && len(frag.Sources) == 1 {
		src = &frag.Sources[0]
	}
	if src == nil {
		return fmt.Errorf("%q is not a source", ref)
	}
	if version != "" && src.Version != version {
		return fmt.Errorf("source %q is version %q, not %q", name, src.Version, version)
	}
	if !sourceReproducible(*src) {
		return fmt.Errorf("source %s is not reproducible from the registry — it needs a url, build recipe, per-file urls, or (tool) a docker:// image or downloadable .sif URL", src.ID())
	}
	clean := stripSourceLocal(*src)
	for i := range clean.Annotations {
		clean.Annotations[i].Default = false // `default` is a local preference
	}
	body, err := registry.RenderSnippet(&config.Snapshot{Sources: []config.Source{clean}})
	if err != nil {
		return err
	}
	itemVersion := src.Version
	title := fmt.Sprintf("source: %s:%s (%s)", src.Name, src.Version, src.Assembly)
	var assets []string
	switch {
	case src.IsTool():
		assets = localAssets(src.Assets)
	case src.Build != nil:
		assets = localAssets(src.Build.Assets)
	}

	issueBody := fmt.Sprintf("Proposed source for the varhub registry (via `varhub registry submit`).\n\n```toml\n%s```\n", body)
	if len(assets) > 0 {
		b64, err := bundleAssets(fragDir, assets)
		if err != nil {
			return err
		}
		issueBody += fmt.Sprintf("\nHelper assets (%d file(s), unpacked next to the fragment by the workflow):\n\n```assets.tar.gz.base64\n%s\n```\n", len(assets), b64)
	}

	if *dryRun {
		fmt.Printf("title: %s\n\n%s", title, issueBody)
		return nil
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		out := fmt.Sprintf("%s_%s.issue.md", name, itemVersion)
		if err := os.WriteFile(out, []byte("# "+title+"\n\n"+issueBody), 0o644); err != nil {
			return err
		}
		fmt.Printf("GITHUB_TOKEN not set — wrote the issue to %s.\n"+
			"Open https://github.com/%s/issues/new and paste it (add the %q label).\n",
			out, config.DefaultRegistryRepo, registry.SubmissionLabel)
		return nil
	}

	url, err := registry.GitHubSubmitter{Token: token}.SubmitIssue(ctx, config.DefaultRegistryRepo, title, issueBody)
	if err != nil {
		return err
	}
	fmt.Printf("opened issue: %s\n", url)
	return nil
}

// localAssets keeps only the co-located (relative, non-URL) asset names — the ones
// that must be bundled into the issue; URL assets travel as-is in the toml.
func localAssets(names []string) []string {
	var out []string
	for _, a := range names {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") || filepath.IsAbs(a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// bundleAssets reads the named files from dir, writes them into a gzip'd tar (flat,
// by basename, mode 0755), and returns the base64 of that archive (wrapped at 76
// columns) for embedding in an issue body. The workflow decodes + unpacks it into the
// fragment dir as individual files.
func bundleAssets(dir string, names []string) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return "", fmt.Errorf("asset %q: %w", n, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(n), Mode: 0o755, Size: int64(len(data))}); err != nil {
			return "", err
		}
		if _, err := tw.Write(data); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return wrap76(base64.StdEncoding.EncodeToString(buf.Bytes())), nil
}

// wrap76 breaks a string into 76-column lines (base64 convention; keeps issue bodies
// readable and paste-safe). Whitespace is ignored by the decoder.
func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteByte('\n')
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

// fetchFragmentAssets downloads a fragment's co-located (relative) helper files —
// a source's build assets and a tool source's step assets — from the registry into
// its version dir, preserving their relative path so recipes/steps resolve them.
// URL/absolute assets are fetched at run time and skipped here.
func fetchFragmentAssets(ctx context.Context, loc, entryFile, dir string, frag *config.Snapshot) (int, error) {
	var names []string
	for _, s := range frag.Sources {
		if s.Build != nil {
			names = append(names, s.Build.Assets...)
		}
		names = append(names, s.Assets...) // tool-source step assets
	}
	n := 0
	for _, a := range names {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") || filepath.IsAbs(a) {
			continue // URL assets are downloaded at run time, not co-located
		}
		data, err := registry.FetchAsset(ctx, loc, entryFile, a)
		if err != nil {
			return n, fmt.Errorf("asset %q: %w", a, err)
		}
		dst := filepath.Join(dir, a)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return n, err
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// stripLocal clears machine-local fields from every source in a fragment (so a
// shared/registry copy carries only the canonical url/checksum).
func stripLocal(frag *config.Snapshot) {
	for i := range frag.Sources {
		frag.Sources[i] = stripSourceLocal(frag.Sources[i])
	}
	frag.Normalize()
}

// stripSourceLocal returns a copy of s with localpath fields cleared (data + index,
// top-level and per-file).
// sourceReproducible reports whether a source can be reconstructed from the registry
// alone (no local files): a canonical URL, a build recipe, a multi-file union where
// every file carries a URL, or — for a type="tool" source — a shareable image (a
// docker://-style ref or a downloadable http(s) .sif URL). A localpath-only file or a
// local .sif can't be shared.
func sourceReproducible(s config.Source) bool {
	if s.IsTool() {
		return s.Image != "" && (s.AsTool().ImageIsRef() ||
			strings.HasPrefix(s.Image, "http://") || strings.HasPrefix(s.Image, "https://"))
	}
	if s.URL != "" || s.Build != nil {
		return true
	}
	if len(s.Files) == 0 {
		return false
	}
	for _, f := range s.Files {
		if f.URL == "" {
			return false
		}
	}
	return true
}

func stripSourceLocal(s config.Source) config.Source {
	s.LocalPath, s.LocalPathIndex = "", ""
	if len(s.Files) > 0 {
		files := make([]config.FileSpec, len(s.Files))
		copy(files, s.Files)
		for i := range files {
			files[i].LocalPath, files[i].LocalPathIndex = "", ""
		}
		s.Files = files
	}
	return s
}
