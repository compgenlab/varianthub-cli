package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/remote"
)

// Driving a VariantHub server from here.
//
// Four commands, and the split from the local ones is meant to be obvious:
// `annotate` runs the engine on this machine against sources on this machine,
// and `submit` sends the variants somewhere else. Folding both into one command
// with a flag would hide the only difference that really matters to somebody
// with patient data in a file.

// parseAnywhere parses flags that may appear after the positional arguments.
//
// Go's flag package stops at the first non-flag argument, so `fetch j-1 -o out`
// leaves -o and out as two more positionals and the command complains that it
// was given three job ids. That is the natural way to type it — the id is the
// subject and the flags are qualifications — and every other tool a person uses
// accepts it, so failing there reads as the command being broken.
//
// Parsing repeatedly is what makes it work: Parse consumes flags until it meets
// a positional, we take that one and hand back the rest, and it picks up any
// flags that followed. Splitting the arguments by hand instead would need to
// know which flags take a value, which is the flag package's job.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// cmdLogin stores a server and token.
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "", "VariantHub base URL, e.g. https://varianthub.example.org")
	token := fs.String("token", "", "API token (omit to be prompted, which keeps it out of shell history)")
	check := fs.Bool("check", true, "verify the server accepts the token before saving")
	profile := fs.String("profile", "", "name this server, so several can be kept (default: \"default\")")
	makeDefault := fs.Bool("default", false, "make this profile the one used when none is named")
	list := fs.Bool("list", false, "show the configured profiles and exit")
	forget := fs.String("forget", "", "remove a profile and exit")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, strings.TrimLeft(`
usage: varhub login --server URL [--token TOKEN] [--profile NAME]
       varhub login --list
       varhub login --forget NAME

Stores a VariantHub server and API token for submit/status/fetch.

Several servers can be kept as named profiles — a production deployment and a
local one, say — and any command takes --profile to pick one. Without it the
file's default is used, or the only profile when there is exactly one.

A token is the only credential this program accepts. There is no
username-and-password path on purpose: a password typed at a command line ends
up in shell history or a CI variable, and it authenticates everything the
account can do. A token is issued for this, revocable on its own, and carries
exactly its owner's rights.

Mint one in the web app under Account -> API tokens.

The token is written to the user config directory (VARHUB_CREDENTIALS to put it
elsewhere), mode 0600 — not VARHUB_HOME, which defaults to the working
directory and is very often a checkout.

VARHUB_SERVER and VARHUB_TOKEN override the stored values for one invocation.
`, "\n"))
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *list {
		return listProfiles()
	}
	if *forget != "" {
		gone, err := remote.Forget(*forget)
		if err != nil {
			return err
		}
		if !gone {
			return fmt.Errorf("no profile %q", *forget)
		}
		fmt.Printf("Removed profile %q\n", *forget)
		return nil
	}

	// The profile being edited, which is also what supplies the half the caller
	// did not give — changing a token should not require restating the server.
	f, err := remote.LoadFile()
	if err != nil {
		return err
	}
	name := f.ProfileName(*profile)
	cur := f.Profiles[name]

	c := remote.Credentials{Server: cur.Server, Token: cur.Token}
	if *server != "" {
		c.Server = strings.TrimRight(strings.TrimSpace(*server), "/")
	}
	if c.Server == "" {
		return fmt.Errorf("--server is required the first time for profile %q", name)
	}

	switch {
	case *token != "":
		c.Token = strings.TrimSpace(*token)
	default:
		// Prompted rather than required as a flag, so the usual path keeps the
		// token out of `ps` and out of shell history.
		fmt.Fprintf(os.Stderr, "API token for %s (profile %s): ", c.Server, name)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return fmt.Errorf("no token given")
		}
		c.Token = strings.TrimSpace(line)
	}
	if c.Token == "" {
		return fmt.Errorf("no token given")
	}

	// Checked before writing, so a typo is a message now rather than a puzzle
	// at the next command.
	if *check {
		if err := remote.New(c).Ping(ctx); err != nil {
			return fmt.Errorf("%w\n(the credentials were not saved)", err)
		}
	}

	path, err := remote.Save(name, c, *makeDefault)
	if err != nil {
		return err
	}
	fmt.Printf("Saved profile %q (%s) to %s\n", name, c.Server, path)
	return nil
}

// listProfiles shows what is configured, and never a token.
//
// The server and which one is default are what somebody needs to see; printing
// the token would put a credential on a terminal, into a scrollback buffer, and
// into whatever captured the output — for a command whose whole purpose is to
// answer "which of these am I using".
func listProfiles() error {
	f, err := remote.LoadFile()
	if err != nil {
		return err
	}
	names := f.Names()
	if len(names) == 0 {
		fmt.Println("No profiles. Run `varhub login --server URL`.")
		return nil
	}
	active := f.ProfileName("")
	for _, n := range names {
		mark := "  "
		if n == active {
			mark = "* "
		}
		fmt.Printf("%s%-16s %s\n", mark, n, f.Profiles[n].Server)
	}
	if v := os.Getenv("VARHUB_SERVER"); v != "" {
		fmt.Fprintf(os.Stderr, "\nnote: VARHUB_SERVER=%s overrides the server above\n", v)
	}
	if os.Getenv("VARHUB_TOKEN") != "" {
		fmt.Fprintln(os.Stderr, "note: VARHUB_TOKEN overrides the token above")
	}
	return nil
}

// cmdSubmit sends variants or a VCF to the server.
func cmdSubmit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "snapshot to annotate against (server-side name)")
	sources := fs.String("sources", "", "comma-separated sources instead of a snapshot; needs --build")
	build := fs.String("build", "", "assembly, required with --sources")
	anns := fs.String("select", "", `annotations: "all", or a comma-separated list (default: the snapshot's)`)
	wait := fs.Bool("wait", false, "poll until the job finishes")
	out := fs.String("o", "", "with --wait, fetch the results here when it finishes")
	format := fs.String("format", "", "with -o: vcf | tsv | csv | json (default: the server's)")
	profile := fs.String("profile", "", "which saved server to use (default: the file's)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, strings.TrimLeft(`
usage: varhub submit [flags] <file.vcf | chrom:pos:ref:alt ...>

Sends variants to a VariantHub server. Prints the job id.

This is the remote counterpart of `+"`annotate`"+`, which runs here against local
sources. The variants leave this machine.
`, "\n"))
		fs.PrintDefaults()
	}
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		fs.Usage()
		return fmt.Errorf("give a VCF file or one or more chrom:pos:ref:alt")
	}

	creds, cerr := remote.Require(*profile)
	if cerr != nil {
		return cerr
	}
	c := remote.New(creds)

	req := remote.SubmitRequest{
		Snapshot: *snapshot, Build: *build, Annotations: *anns,
	}
	if *sources != "" {
		req.Sources = splitComma(*sources)
	}
	// One argument that exists as a file is a VCF; anything else is a locus
	// list. Told by the filesystem rather than by the extension, so a
	// ".vcf.gz", a ".bcf" and a file with no extension all work, and a locus
	// that happens to look like a path does not silently become a missing file.
	if len(rest) == 1 {
		if _, statErr := os.Stat(rest[0]); statErr == nil {
			req.VCFPath = rest[0]
		}
	}
	if req.VCFPath == "" {
		req.Variants = rest
	}

	id, err := c.Submit(ctx, req)
	if err != nil {
		return err
	}
	fmt.Println(id)
	if !*wait {
		fmt.Fprintf(os.Stderr, "varhub status %s\n", id)
		return nil
	}

	j, err := waitFor(ctx, c, id)
	if err != nil {
		return err
	}
	if j.Status != "done" {
		return jobFailed(j)
	}
	if *out == "" {
		return nil
	}
	return fetchTo(ctx, c, id, *format, *out)
}

// cmdStatus reports on a job.
func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	wait := fs.Bool("wait", false, "poll until the job finishes")
	profile := fs.String("profile", "", "which saved server to use (default: the file's)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: varhub status [--wait] [--profile NAME] <job-id>\n")
		fs.PrintDefaults()
	}
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("give one job id")
	}
	creds, err := remote.Require(*profile)
	if err != nil {
		return err
	}
	c := remote.New(creds)

	j, err := c.Status(ctx, rest[0])
	if err != nil {
		return err
	}
	if *wait && !j.Terminal() {
		if j, err = waitFor(ctx, c, rest[0]); err != nil {
			return err
		}
	}
	printJob(j)
	if j.Status == "error" {
		return jobFailed(j)
	}
	return nil
}

// cmdFetch downloads a finished job's results.
func cmdFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	format := fs.String("format", "", "vcf | tsv | csv | json (default: the server's, which is vcf for a VCF job)")
	out := fs.String("o", "-", "write here; - is stdout")
	profile := fs.String("profile", "", "which saved server to use (default: the file's)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, strings.TrimLeft(`
usage: varhub fetch [--format F] [-o FILE] <job-id>

Downloads a finished job's results. vcf comes back BGZF-compressed, and where
the server's storage is reachable it is fetched straight from there rather than
relayed — so this is the cheap way to move a large result.
`, "\n"))
		fs.PrintDefaults()
	}
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		fs.Usage()
		return fmt.Errorf("give one job id")
	}
	creds, err := remote.Require(*profile)
	if err != nil {
		return err
	}
	return fetchTo(ctx, remote.New(creds), rest[0], *format, *out)
}

// fetchTo streams a result to a path, or to stdout for "-".
//
// Written to a temporary name and renamed, so an interrupted download does not
// leave a truncated file sitting where a whole one should be — which for a BGZF
// VCF reads as a valid file that simply ends early.
func fetchTo(ctx context.Context, c *remote.Client, id, format, out string) error {
	if out == "" || out == "-" {
		_, err := c.Fetch(ctx, id, format, os.Stdout)
		return err
	}
	tmp := out + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := c.Fetch(ctx, id, format, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", out, n)
	return nil
}

// waitFor polls until the job settles.
//
// Backing off from a second to fifteen. A job that finishes quickly is noticed
// quickly; one that takes an hour is not asked about three thousand times.
func waitFor(ctx context.Context, c *remote.Client, id string) (remote.Job, error) {
	delay := time.Second
	for {
		j, err := c.Status(ctx, id)
		if err != nil {
			return remote.Job{}, err
		}
		if j.Terminal() {
			return j, nil
		}
		printProgress(j)
		select {
		case <-ctx.Done():
			return remote.Job{}, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay += 2 * time.Second
		}
	}
}

func printProgress(j remote.Job) {
	if j.Total > 1 {
		fmt.Fprintf(os.Stderr, "\r%s %d/%d chunks", j.Status, j.Done, j.Total)
		return
	}
	fmt.Fprintf(os.Stderr, "\r%s", j.Status)
}

func printJob(j remote.Job) {
	fmt.Printf("job       %s\n", j.JobID)
	fmt.Printf("status    %s\n", j.Status)
	fmt.Printf("kind      %s\n", j.Kind)
	if j.Snapshot != "" {
		fmt.Printf("snapshot  %s\n", j.Snapshot)
	}
	if j.Total > 1 {
		fmt.Printf("chunks    %d done, %d failed, of %d\n", j.Done, j.Failed, j.Total)
	}
	if j.NVariants > 0 {
		fmt.Printf("variants  %d\n", j.NVariants)
	}
	if j.Runner != "" {
		fmt.Printf("ran on    %s\n", j.Runner)
	}
	if j.CreatedAt > 0 {
		fmt.Printf("submitted %s\n", time.Unix(j.CreatedAt, 0).Format(time.RFC3339))
	}
	if j.FinishedAt > 0 {
		fmt.Printf("finished  %s\n", time.Unix(j.FinishedAt, 0).Format(time.RFC3339))
	}
	// Said plainly, because "no results" and "results expired" look identical
	// otherwise and lead somewhere different: one means resubmit, the other
	// means there was nothing to find.
	if j.PurgedAt > 0 {
		fmt.Printf("results   expired %s; the record of the run is kept\n",
			time.Unix(j.PurgedAt, 0).Format(time.RFC3339))
	}
	if j.Error != "" {
		fmt.Printf("error     %s\n", j.Error)
	}
}

// jobFailed turns a failed job into a non-zero exit with the server's reason.
func jobFailed(j remote.Job) error {
	if j.Error != "" {
		return fmt.Errorf("job %s %s: %s", j.JobID, j.Status, j.Error)
	}
	return fmt.Errorf("job %s %s", j.JobID, j.Status)
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
