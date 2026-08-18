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
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultProfile is the profile used when nothing names one.
const DefaultProfile = "default"

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

// File is the whole credentials file: several servers, and which one is meant
// by default.
//
// Profiles because one person routinely has more than one of these — a
// production deployment and a local one, or their institution's and a
// collaborator's — and the alternative is retyping --server and --token, or
// keeping two files and swapping VARHUB_CREDENTIALS between them. Both of those
// end with a token pasted into a shell.
//
// Server and Token at the top level are the older single-server layout, kept
// readable so an existing file does not stop working. See Resolve.
type File struct {
	Default  string                 `toml:"default"`
	Profiles map[string]Credentials `toml:"profiles"`

	Server string `toml:"server"`
	Token  string `toml:"token"`
}

// CredentialsPath is where the tokens are kept.
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

// LoadFile reads the credentials file. A missing file is an empty one.
func LoadFile() (File, error) {
	var f File
	path, err := CredentialsPath()
	if err != nil {
		return f, nil // nowhere to look is not a failure; Require reports it
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return f, nil
	}
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return File{}, fmt.Errorf("read %s: %w", path, err)
	}
	warnIfReadable(path)

	// The single-server layout, folded in as the default profile so everything
	// downstream sees one shape. Only when no profile of that name was written
	// explicitly, which would otherwise be silently overwritten by a stanza the
	// person had moved away from.
	if f.Server != "" || f.Token != "" {
		if f.Profiles == nil {
			f.Profiles = map[string]Credentials{}
		}
		if _, ok := f.Profiles[DefaultProfile]; !ok {
			f.Profiles[DefaultProfile] = Credentials{Server: f.Server, Token: f.Token}
			// Named as the default too, not merely present. The older layout has
			// no default key, so without this the file reads as "has profiles,
			// names no default" — and the next profile saved takes the title,
			// silently pointing every later command at a server the person only
			// added as a second one.
			if f.Default == "" {
				f.Default = DefaultProfile
			}
		}
	}
	return f, nil
}

// ProfileName resolves which profile is meant.
//
// Explicit flag, then VARHUB_PROFILE, then the file's own default, then the
// conventional name. A file holding exactly one profile answers with it
// whatever it is called: naming a lone profile is a formality, and refusing
// because its name is not "default" would be pedantry with a wrong answer
// attached.
func (f File) ProfileName(want string) string {
	if want = strings.TrimSpace(want); want != "" {
		return want
	}
	if v := strings.TrimSpace(os.Getenv("VARHUB_PROFILE")); v != "" {
		return v
	}
	if f.Default != "" {
		return f.Default
	}
	if len(f.Profiles) == 1 {
		for name := range f.Profiles {
			return name
		}
	}
	return DefaultProfile
}

// Names lists the configured profiles, in order.
func (f File) Names() []string {
	out := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ErrNoSuchProfile is returned when a named profile is not in the file.
type ErrNoSuchProfile struct {
	Name  string
	Known []string
}

func (e ErrNoSuchProfile) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("no profile %q; none are configured. "+
			"Run `varhub login --server URL --profile %s`", e.Name, e.Name)
	}
	return fmt.Sprintf("no profile %q; this file has %s",
		e.Name, strings.Join(e.Known, ", "))
}

// Load returns one profile's credentials, with the environment applied on top.
//
// The environment wins because that is what the environment is for: a CI job or
// a container sets VARHUB_TOKEN and should not also have to write a file, and a
// person overriding one call should not have to edit and restore their saved
// settings around it.
//
// Applied per field rather than all-or-nothing, so VARHUB_TOKEN alone can point
// a saved server at a different credential — which is the shape a CI job
// actually has, the server being the stable half.
//
// A profile that is not in the file is not an error here when the environment
// supplies what is needed; asking for a name that exists nowhere and getting
// nothing from the environment either is.
func Load(profile string) (Credentials, error) {
	f, err := LoadFile()
	if err != nil {
		return Credentials{}, err
	}
	name := f.ProfileName(profile)
	c, ok := f.Profiles[name]

	envServer := strings.TrimSpace(os.Getenv("VARHUB_SERVER"))
	envToken := strings.TrimSpace(os.Getenv("VARHUB_TOKEN"))
	if envServer != "" {
		c.Server = envServer
	}
	if envToken != "" {
		c.Token = envToken
	}

	// Named explicitly, absent from the file, and not rescued by the
	// environment: say so rather than failing later against an empty address,
	// which reads as the server being down.
	if !ok && strings.TrimSpace(profile) != "" && (c.Server == "" || c.Token == "") {
		return Credentials{}, ErrNoSuchProfile{Name: name, Known: f.Names()}
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

// Require loads a profile and fails if it cannot be used.
func Require(profile string) (Credentials, error) {
	c, err := Load(profile)
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

// Save writes one profile, leaving the others as they were.
//
// Read-modify-write rather than a plain overwrite, which is the whole
// difference between adding a second server and replacing the first. makeDefault
// is forced for the first profile in a file: a file with profiles and no default
// would resolve to "default" and find nothing.
func Save(profile string, c Credentials, makeDefault bool) (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	f, err := LoadFile()
	if err != nil {
		return "", err
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Credentials{}
	}
	name := strings.TrimSpace(profile)
	if name == "" {
		name = f.ProfileName("")
	}

	first := len(f.Profiles) == 0
	f.Profiles[name] = Credentials{
		Server: strings.TrimRight(strings.TrimSpace(c.Server), "/"),
		Token:  strings.TrimSpace(c.Token),
	}
	if makeDefault || first || f.Default == "" {
		f.Default = name
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeFile(path, f); err != nil {
		return "", err
	}
	return path, nil
}

// Forget removes a profile. Reports whether there was one.
func Forget(profile string) (bool, error) {
	path, err := CredentialsPath()
	if err != nil {
		return false, err
	}
	f, err := LoadFile()
	if err != nil {
		return false, err
	}
	name := strings.TrimSpace(profile)
	if _, ok := f.Profiles[name]; !ok {
		return false, nil
	}
	delete(f.Profiles, name)
	// A default pointing at a profile that is gone resolves to nothing, which
	// reads as "no server configured" while the file plainly has one. Move it to
	// the survivor when there is exactly one, and clear it otherwise.
	if f.Default == name {
		f.Default = ""
		if n := f.Names(); len(n) == 1 {
			f.Default = n[0]
		}
	}
	return true, writeFile(path, f)
}

// writeFile renders the file and replaces it atomically, owner-readable only.
//
// The single-server keys are not written back. A file that carried both would
// have two answers for the default profile, and the fold-in on read prefers the
// stanza — so the top-level pair would sit there looking authoritative and
// meaning nothing.
func writeFile(path string, f File) error {
	var b strings.Builder
	b.WriteString("# varhub credentials. Keep this file to yourself.\n")
	b.WriteString("# Profiles are servers you use; --profile or VARHUB_PROFILE picks one.\n")
	b.WriteString("# VARHUB_SERVER and VARHUB_TOKEN override whichever is chosen.\n\n")
	if f.Default != "" {
		fmt.Fprintf(&b, "default = %q\n\n", f.Default)
	}
	for _, name := range f.Names() {
		c := f.Profiles[name]
		fmt.Fprintf(&b, "[profiles.%s]\n", name)
		fmt.Fprintf(&b, "  server = %q\n", c.Server)
		fmt.Fprintf(&b, "  token  = %q\n\n", c.Token)
	}

	// Written and then renamed, so an interrupted write cannot leave a
	// half-file where a token was, and so the new content never exists at the
	// final path with the wrong mode.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Re-applied after the rename, because the mode that matters is the one on
	// the file that ends up holding the tokens — one created by hand, or
	// restored from a backup that lost its mode, is exactly the one nobody
	// thinks to check.
	return os.Chmod(path, 0o600)
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
			"warning: %s holds API tokens and is readable by others (%04o); "+
				"chmod 600 it\n", path, m)
	}
}
