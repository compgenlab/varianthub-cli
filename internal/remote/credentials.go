// Package remote talks to a VariantHub server.
//
// varhub has always annotated locally: it reads a snapshot from disk, runs the
// engine in-process, and writes the answer. That is the whole of it, and it is
// the right shape for one machine with the source data on it.
//
// A VariantHub deployment is the other arrangement — the sources live there,
// the work runs there, and what a caller has is a token. Until now nothing in
// this program could reach one, so the published REST API's intended consumer
// existed and its client did not. This is the client.
//
// The split is deliberate and stays visible in the commands: `annotate` runs
// here, `submit` runs there. Making one command silently mean either would hide
// the difference that matters most — whether the variants leave the machine.
package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Credentials are where a server is and what to present to it.
//
// A token and nothing else. There is no username-and-password path from the
// command line, by design: a password typed into a CLI ends up in shell
// history, in a script, or in a CI variable that outlives the person who set
// it, and it authenticates *everything* the account can do rather than one
// program. A token is issued for this purpose, is revocable on its own, and
// carries exactly its owner's rights.
type Credentials struct {
	Server string `toml:"server"`
	Token  string `toml:"token"`
}

// CredentialsPath is where the token is kept.
//
// Under the user's config directory rather than VARHUB_HOME, which is the one
// place this package deliberately breaks with the rest of the program.
// VARHUB_HOME defaults to the working directory, and a working directory is
// very often a checkout — so the ordinary default would write a bearer token
// into whatever repository somebody happened to be standing in, where the next
// `git add -A` commits it.
//
// VARHUB_CREDENTIALS overrides it for anyone who wants it elsewhere.
func CredentialsPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("VARHUB_CREDENTIALS")); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no config directory to store credentials in: %w", err)
	}
	return filepath.Join(dir, "varhub", "credentials.toml"), nil
}

// Load reads the stored credentials, with the environment taking precedence.
//
// The environment wins because that is what the environment is for: a CI job or
// a container sets VARHUB_TOKEN and should not also have to write a file, and a
// person overriding one call should not have to edit and restore their saved
// settings around it.
//
// Missing is not an error here. The caller knows whether it needs a server —
// `login` is about to write one — so reporting that is left to Require.
func Load() (Credentials, error) {
	var c Credentials

	path, err := CredentialsPath()
	if err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			if _, err := toml.DecodeFile(path, &c); err != nil {
				return Credentials{}, fmt.Errorf("read %s: %w", path, err)
			}
			warnIfReadable(path)
		}
	}

	if v := strings.TrimSpace(os.Getenv("VARHUB_SERVER")); v != "" {
		c.Server = v
	}
	if v := strings.TrimSpace(os.Getenv("VARHUB_TOKEN")); v != "" {
		c.Token = v
	}
	c.Server = strings.TrimRight(strings.TrimSpace(c.Server), "/")
	c.Token = strings.TrimSpace(c.Token)
	return c, nil
}

// ErrNoServer is returned when nothing says which server to talk to.
var ErrNoServer = errors.New(
	"no VariantHub server configured; run `varhub login --server URL` or set VARHUB_SERVER")

// ErrNoToken is returned when there is a server but no credential for it.
var ErrNoToken = errors.New(
	"no API token; run `varhub login` or set VARHUB_TOKEN")

// Require loads credentials and fails if they cannot be used.
func Require() (Credentials, error) {
	c, err := Load()
	if err != nil {
		return Credentials{}, err
	}
	if c.Server == "" {
		return Credentials{}, ErrNoServer
	}
	if c.Token == "" {
		return Credentials{}, ErrNoToken
	}
	return c, nil
}

// Save writes the credentials to disk, readable only by their owner.
//
// The file is created 0600 and an existing one is re-chmodded, because the mode
// that matters is the one on the file that ends up holding the token — a file
// somebody created by hand, or restored from a backup that lost its mode, is
// exactly the one nobody thinks to check.
func Save(c Credentials) (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# varhub credentials. Keep this file to yourself.\n")
	b.WriteString("# Overridden by VARHUB_SERVER and VARHUB_TOKEN.\n")
	fmt.Fprintf(&b, "server = %q\n", strings.TrimRight(c.Server, "/"))
	fmt.Fprintf(&b, "token  = %q\n", c.Token)

	// Written and then renamed, so an interrupted write cannot leave a
	// half-file where a token was, and so the new content never exists at the
	// final path with the wrong mode.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// warnIfReadable says so when the token file is open to anyone else.
//
// A warning rather than a refusal: the file is readable and the command would
// work, and failing over the mode would leave somebody unable to run anything
// while they worked out why. Saying it every time is the point — a token
// readable by the whole machine is worth being nagged about.
func warnIfReadable(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if m := info.Mode().Perm(); m&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %s holds an API token and is readable by others (%04o); "+
				"chmod 600 it\n", path, m)
	}
}
