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
	if _, err := Save("", Credentials{Server: "https://vh.example.org/", Token: "tok-1"}, false); err != nil {
		t.Fatal(err)
	}
	got, err := Load("")
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
	if _, err := Save("", Credentials{Server: "https://vh.example.org", Token: "tok-1"}, false); err != nil {
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
	if _, err := Save("", Credentials{Server: "https://vh.example.org", Token: "tok-1"}, false); err != nil {
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
	if _, err := Save("", Credentials{Server: "https://saved.example.org", Token: "saved"}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARHUB_SERVER", "https://env.example.org")
	t.Setenv("VARHUB_TOKEN", "env-token")

	got, err := Load("")
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
	if _, err := Require(""); err == nil || !strings.Contains(err.Error(), "varhub login") {
		t.Errorf("no server gave %v, want advice to log in", err)
	}

	t.Setenv("VARHUB_SERVER", "https://vh.example.org")
	_, err := Require("")
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

// --- profiles ---

// Several servers is the ordinary case — a production deployment and a local
// one — and saving the second must not replace the first.
func TestASecondProfileDoesNotReplaceTheFirst(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}

	f, err := LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Names(); len(got) != 2 {
		t.Fatalf("profiles = %v, want two", got)
	}
	prod, err := Load("prod")
	if err != nil {
		t.Fatal(err)
	}
	if prod.Server != "https://prod.example.org" || prod.Token != "p" {
		t.Errorf("prod = %+v", prod)
	}
	dev, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Server != "http://localhost:8080" || dev.Token != "d" {
		t.Errorf("dev = %+v", dev)
	}
}

// The first profile saved becomes the default, or a file with profiles and no
// default resolves to the name "default" and finds nothing in it.
func TestTheFirstProfileBecomesTheDefault(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}

	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://prod.example.org" {
		t.Errorf("the unnamed profile resolved to %q, want the first one saved", got.Server)
	}

	// And --default moves it.
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = Load(""); got.Server != "http://localhost:8080" {
		t.Errorf("after --default the unnamed profile is %q", got.Server)
	}
}

// A lone profile answers whatever it is called. Naming it is a formality, and
// refusing because it is not called "default" would be pedantry with a wrong
// answer attached.
func TestALoneProfileIsUsedWhateverItIsNamed(t *testing.T) {
	path := isolate(t)
	if err := os.WriteFile(path, []byte(
		"[profiles.work]\n  server = \"https://work.example.org\"\n  token = \"w\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://work.example.org" {
		t.Errorf("a lone profile did not answer: %+v", got)
	}
}

// VARHUB_PROFILE picks one without a flag, which is how a shell session or a CI
// job pins itself to a server for every command that follows.
func TestTheEnvironmentCanPickTheProfile(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARHUB_PROFILE", "dev")

	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "http://localhost:8080" {
		t.Errorf("VARHUB_PROFILE was ignored: %+v", got)
	}
}

// The flag beats the environment, so one command can be pointed elsewhere
// without unsetting anything.
func TestTheFlagBeatsTheEnvironmentProfile(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARHUB_PROFILE", "dev")

	got, err := Load("prod")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://prod.example.org" {
		t.Errorf("--profile did not win: %+v", got)
	}
}

// VARHUB_TOKEN alone points a saved server at another credential, which is the
// shape a CI job has — the server being the stable half.
func TestATokenFromTheEnvironmentKeepsTheSavedServer(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "saved"}, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARHUB_TOKEN", "from-ci")

	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://prod.example.org" || got.Token != "from-ci" {
		t.Errorf("got %+v, want the saved server with the environment's token", got)
	}
}

// Asking for a profile that is not there says which ones are, rather than
// failing later against an empty address — which reads as the server being down.
func TestAnUnknownProfileNamesTheOnesThatExist(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, true); err != nil {
		t.Fatal(err)
	}
	_, err := Load("staging")
	if err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	if !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "prod") {
		t.Errorf("the error names neither the missing profile nor the real one: %v", err)
	}
}

// The single-server layout keeps working, folded in as the default profile.
func TestTheOlderSingleServerFileStillWorks(t *testing.T) {
	path := isolate(t)
	if err := os.WriteFile(path, []byte(
		"server = \"https://old.example.org\"\ntoken = \"old-token\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://old.example.org" || got.Token != "old-token" {
		t.Errorf("the older layout stopped resolving: %+v", got)
	}

	// And saving beside it migrates rather than duplicating: the rewritten file
	// carries profiles, not the old top-level pair, so there is one answer.
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\nserver =") {
		t.Errorf("the rewritten file still carries a top-level server:\n%s", b)
	}
	if again, _ := Load(""); again.Server != "https://old.example.org" {
		t.Errorf("the migrated default changed to %q", again.Server)
	}
}

// Removing a profile must not leave the default pointing at nothing, which
// reads as "no server configured" while the file plainly has one.
func TestForgettingTheDefaultMovesItToTheSurvivor(t *testing.T) {
	isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}

	gone, err := Forget("prod")
	if err != nil || !gone {
		t.Fatalf("forget: gone=%v err=%v", gone, err)
	}
	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "http://localhost:8080" {
		t.Errorf("after forgetting the default, the unnamed profile is %+v", got)
	}
	if _, err := Load("prod"); err == nil {
		t.Error("the forgotten profile still resolves")
	}
}

// Every profile's token stays private, not just the first one written.
func TestAMultiProfileFileIsStillPrivate(t *testing.T) {
	path := isolate(t)
	if _, err := Save("prod", Credentials{Server: "https://prod.example.org", Token: "p"}, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("dev", Credentials{Server: "http://localhost:8080", Token: "d"}, false); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if m := info.Mode().Perm(); m&0o077 != 0 {
		t.Errorf("credentials left at mode %04o after adding a profile", m)
	}
}
