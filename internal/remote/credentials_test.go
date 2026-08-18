package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the credentials file somewhere disposable.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.toml")
	t.Setenv("VARHUB_CREDENTIALS", path)
	t.Setenv("VARHUB_SERVER", "")
	t.Setenv("VARHUB_TOKEN", "")
	return path
}

func TestSavedCredentialsRoundTrip(t *testing.T) {
	isolate(t)
	if _, err := Save(Credentials{Server: "https://vh.example.org/", Token: "tok-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// The trailing slash is dropped, or every path this builds carries a double
	// separator and the server answers 404 for reasons nobody would guess.
	if got.Server != "https://vh.example.org" {
		t.Errorf("server = %q", got.Server)
	}
	if got.Token != "tok-1" {
		t.Errorf("token = %q", got.Token)
	}
}

// A file holding a bearer token must not be readable by anyone else.
func TestTheTokenFileIsPrivate(t *testing.T) {
	path := isolate(t)
	if _, err := Save(Credentials{Server: "https://vh.example.org", Token: "tok-1"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := info.Mode().Perm(); m&0o077 != 0 {
		t.Errorf("credentials are mode %04o; a token must not be readable by others", m)
	}
}

// Saving over a file somebody left world-readable has to fix the mode. That
// file — created by hand, or restored from a backup that lost its mode — is
// exactly the one nobody thinks to check.
func TestSavingTightensAnAlreadyLooseFile(t *testing.T) {
	path := isolate(t)
	if err := os.WriteFile(path, []byte("server=\"\"\ntoken=\"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(Credentials{Server: "https://vh.example.org", Token: "tok-1"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if m := info.Mode().Perm(); m&0o077 != 0 {
		t.Errorf("credentials left at mode %04o after a save", m)
	}
}

// The environment wins, so a CI job or a one-off override needs no file and
// leaves no file behind.
func TestTheEnvironmentOverridesTheStoredCredentials(t *testing.T) {
	isolate(t)
	if _, err := Save(Credentials{Server: "https://saved.example.org", Token: "saved"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARHUB_SERVER", "https://env.example.org")
	t.Setenv("VARHUB_TOKEN", "env-token")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://env.example.org" || got.Token != "env-token" {
		t.Errorf("the environment did not win: %+v", got)
	}
}

// Missing credentials say which command fixes them, rather than failing at the
// first request with a connection error to the empty string.
func TestRequireNamesWhatIsMissing(t *testing.T) {
	isolate(t)
	if _, err := Require(); err == nil || !strings.Contains(err.Error(), "varhub login") {
		t.Errorf("no server gave %v, want advice to log in", err)
	}

	t.Setenv("VARHUB_SERVER", "https://vh.example.org")
	_, err := Require()
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("a server with no token gave %v", err)
	}
}

// The token does not go in VARHUB_HOME, which defaults to the working
// directory. That default would write a bearer credential into whatever
// checkout somebody happened to be standing in, where `git add -A` commits it.
func TestTheTokenIsNotStoredUnderVarhubHome(t *testing.T) {
	os.Unsetenv("VARHUB_CREDENTIALS")
	home := t.TempDir()
	t.Setenv("VARHUB_HOME", home)

	path, err := CredentialsPath()
	if err != nil {
		t.Skipf("no user config directory on this system: %v", err)
	}
	if strings.HasPrefix(path, home) {
		t.Errorf("credentials would be written to %s, inside VARHUB_HOME (%s)", path, home)
	}
	if !strings.Contains(path, "varhub") {
		t.Errorf("credentials path %q does not name the program", path)
	}
}
