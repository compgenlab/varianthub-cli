// Package registry fetches a remote catalog of source/snapshot configs over HTTP
// (raw GitHub, no git dependency). The registry holds configurations, not data:
// `cganno download` fetches the actual files afterward. It is a convenience
// starting point — users own and edit their local snapshots.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/compgenlab/cganno/internal/config"
)

// Entry is one catalog item (a snapshot, source, or tool).
type Entry struct {
	Name        string `toml:"name"`
	Version     string `toml:"version"`            // sources only
	Title       string `toml:"title,omitempty"`    // human-readable name (falls back to Name)
	Assembly    string `toml:"assembly,omitempty"` // sources only: genome assembly (e.g. GRCh38)
	File        string `toml:"file"`               // path (relative to the registry base) of the config
	Description string `toml:"description"`
	// NonCommercial marks a source whose data/annotations are restricted to
	// non-commercial use (informational; shown by `registry list`).
	NonCommercial bool `toml:"non_commercial,omitempty"`
	// Latest marks this as the most-recent version of the source, resolved when a
	// ref omits the version or uses ":latest". Versions aren't reliably sortable
	// (semver 1.3, dbSNP b157, dates), so the publisher declares latest — like a
	// docker `latest` tag. At most one entry per source name should set it.
	Latest bool `toml:"latest,omitempty"`
}

// DisplayTitle is the entry's human-readable name (Title), falling back to Name.
func (e Entry) DisplayTitle() string {
	if e.Title != "" {
		return e.Title
	}
	return e.Name
}

// Manifest is the registry.toml at the registry base URL. Tool sources (type="tool")
// live under [[sources]] like any other source — there is no separate [[tools]].
type Manifest struct {
	Snapshots []Entry `toml:"snapshots"`
	Sources   []Entry `toml:"sources"`
}

// Fetch retrieves and parses a registry manifest. loc is an HTTPS URL to a
// registry.toml (used as-is when it ends in .toml), or a base URL (registry.toml
// is appended).
func Fetch(ctx context.Context, loc string) (*Manifest, error) {
	data, err := get(ctx, manifestURL(loc))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("parse registry.toml: %w", err)
	}
	return &m, nil
}

// FetchFragment retrieves and decodes a source fragment file, resolved relative
// to the manifest's directory, and normalizes its derived annotation list.
func FetchFragment(ctx context.Context, loc, file string) (*config.Snapshot, error) {
	data, err := get(ctx, join(baseDir(manifestURL(loc)), file))
	if err != nil {
		return nil, err
	}
	var snap config.Snapshot
	if _, err := toml.Decode(string(data), &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	snap.Normalize()
	return &snap, nil
}

// FetchFile retrieves a file (relative to the manifest's directory) as raw bytes —
// e.g. a snapshot manifest, which is parsed by the caller, not as a fragment.
func FetchFile(ctx context.Context, loc, file string) ([]byte, error) {
	return get(ctx, join(baseDir(manifestURL(loc)), file))
}

// FetchAsset retrieves a build asset co-located with a source fragment in the
// registry: relPath is resolved against the fragment file's directory (so a
// source's [[sources.build]] assets ship alongside its TOML).
func FetchAsset(ctx context.Context, loc, fragmentFile, relPath string) ([]byte, error) {
	fragURL := join(baseDir(manifestURL(loc)), fragmentFile)
	return get(ctx, join(baseDir(fragURL), relPath))
}

// manifestURL resolves a registry location to its registry.toml URL.
func manifestURL(loc string) string {
	if strings.HasSuffix(loc, ".toml") {
		return loc
	}
	return join(loc, "registry.toml")
}

// baseDir returns a URL minus its last path segment (for resolving entry files).
func baseDir(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[:i]
	}
	return u
}

// Snapshot finds a snapshot (manifest bundle) entry by name.
func (m *Manifest) Snapshot(name string) *Entry {
	for i := range m.Snapshots {
		if m.Snapshots[i].Name == name {
			return &m.Snapshots[i]
		}
	}
	return nil
}

// Source / SourceE resolve a source entry (data or tool) by name and version. An
// empty version or "latest" resolves to the entry flagged `latest = true`; if none is
// flagged but the name has exactly one version, that one is used. The …E form returns
// an explicit error for the ambiguous case (several versions, none latest, no version
// requested).
func (m *Manifest) Source(name, version string) *Entry {
	e, _ := entryE(m.Sources, name, version)
	return e
}
func (m *Manifest) SourceE(name, version string) (*Entry, error) {
	return entryE(m.Sources, name, version)
}

// entryE resolves one entry from a list by name+version (with latest/sole handling).
func entryE(entries []Entry, name, version string) (*Entry, error) {
	if version == "" || version == "latest" {
		var sole *Entry
		n := 0
		for i := range entries {
			if entries[i].Name != name {
				continue
			}
			if entries[i].Latest {
				return &entries[i], nil
			}
			sole = &entries[i]
			n++
		}
		if n == 1 {
			return sole, nil
		}
		if n > 1 {
			return nil, fmt.Errorf("multiple versions of %q — specify one (%s:<version>) or mark one latest=true", name, name)
		}
		return nil, nil
	}
	for i := range entries {
		if entries[i].Name == name && entries[i].Version == version {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// SubmissionLabel marks issues that the registry's curation workflow converts to PRs.
const SubmissionLabel = "source-submission"

// RenderSnippet marshals a (one-source) snapshot fragment to TOML for an issue
// body, with the same formatting as written fragments (long arrays reflowed).
func RenderSnippet(snap *config.Snapshot) (string, error) {
	s, err := config.MarshalSnapshot(snap)
	if err != nil {
		return "", fmt.Errorf("render snippet: %w", err)
	}
	return s, nil
}

// Submitter opens a source-contribution request against a registry repo. The
// default impl posts a GitHub issue; a relay-backed impl can replace it later.
type Submitter interface {
	SubmitIssue(ctx context.Context, repo, title, body string) (url string, err error)
}

// GitHubSubmitter opens issues via the GitHub REST API with a bearer token.
type GitHubSubmitter struct {
	Token   string
	APIBase string // defaults to https://api.github.com (overridable for tests)
}

// SubmitIssue creates an issue (labeled SubmissionLabel) on repo (owner/name) and
// returns its html_url.
func (g GitHubSubmitter) SubmitIssue(ctx context.Context, repo, title, body string) (string, error) {
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	payload, err := json.Marshal(map[string]any{
		"title": title, "body": body, "labels": []string{SubmissionLabel},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/repos/"+repo+"/issues", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create issue on %s: %s: %s", repo, resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func join(base, rel string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(rel, "/")
}
