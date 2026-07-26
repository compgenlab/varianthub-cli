// Package tool runs external (often containerized) annotators such as VEP. A
// tool is an ordered list of templated shell steps that transform an input VCF
// into an annotated output file (tab/vcf); the output is then consumed as a
// normal source. Each step runs as a local subprocess; container steps are
// wrapped with the tool's container engine (apptainer/singularity).
package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/htsio/tabix"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// Params carry the values substituted into step templates.
type Params struct {
	Input   string // input VCF
	Output  string // final output file the last step writes
	Workdir string // shared scratch dir between steps
	Image   string // resolved (cached) container image path
	Ref     string // reference FASTA
	Datadir string // the tool's persistent data dir ({datadir}); bound into container steps
	Chrom   string // optional chromosome filter
	// AssetDir is the tool fragment's directory (the snapshot dir): declared
	// tool.Assets are resolved relative to it and staged into Workdir before steps
	// run (see stageAssets). Empty when the tool declares no relative assets.
	AssetDir string
}

// Run executes a tool's steps in workdir, producing p.Output, and ensures the
// output is tabix-indexed.
func Run(ctx context.Context, t config.Tool, p Params) error {
	if len(t.Steps) == 0 {
		return fmt.Errorf("tool %s: no steps defined", t.ID())
	}
	warnMissingAssets(t, t.Steps)
	if err := stageAssets(t, p); err != nil {
		return fmt.Errorf("tool %s: %w", t.ID(), err)
	}
	for i, step := range t.Steps {
		if err := runStep(ctx, t, step, i, p); err != nil {
			return fmt.Errorf("tool %s: step %q: %w", t.ID(), stepLabel(step, i), err)
		}
	}
	if err := ensureIndex(p.Output, t.Format); err != nil {
		return fmt.Errorf("tool %s: index output: %w", t.ID(), err)
	}
	return nil
}

// PullImage pulls a registry-ref image (docker://, …) to dest via the tool's
// container engine (e.g. `apptainer pull <dest> docker://…`).
func PullImage(ctx context.Context, t config.Tool, dest string) error {
	if err := exec1(ctx, filepath.Dir(dest), t.ContainerEngine(), "pull", dest, t.Image); err != nil {
		return fmt.Errorf("tool %s: pull %s: %w", t.ID(), t.Image, err)
	}
	return nil
}

// Setup runs a tool's one-time setup steps (e.g. VEP's INSTALL.pl), installing the
// tool's data into p.Datadir. Container setup steps run inside the image with the
// data dir bound. No-op when the tool has no setup.
func Setup(ctx context.Context, t config.Tool, p Params) error {
	warnMissingAssets(t, t.Setup)
	if err := stageAssets(t, p); err != nil {
		return fmt.Errorf("tool %s: %w", t.ID(), err)
	}
	for i, step := range t.Setup {
		if err := runStep(ctx, t, step, i, p); err != nil {
			return fmt.Errorf("tool %s: setup step %q: %w", t.ID(), stepLabel(step, i), err)
		}
	}
	return nil
}

// stageAssets copies the tool's declared helper files (config.Tool.Assets, co-located
// with the fragment in p.AssetDir) into the step workdir, so a step can reference one
// as `{workdir}/<name>` — in host steps directly, and in container steps via the
// workdir bind at /varhub/work. Staged files are made executable. A missing asset is a
// clear error. No-op when the tool declares no assets.
func stageAssets(t config.Tool, p Params) error {
	for _, a := range t.Assets {
		src := a
		if !filepath.IsAbs(src) {
			if p.AssetDir == "" {
				return fmt.Errorf("asset %q: no fragment dir to resolve it against", a)
			}
			src = filepath.Join(p.AssetDir, a)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("asset %q: %w", a, err)
		}
		dst := filepath.Join(p.Workdir, filepath.Base(a))
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return fmt.Errorf("asset %q: %w", a, err)
		}
	}
	return nil
}

// Recognize {workdir}/<file> used as a script or stdin input (must pre-exist) vs.
// produced as an output (-o / > / >>). A workdir-relative filename stops at shell
// metacharacters so pipelines parse.
var (
	wdScriptRef = regexp.MustCompile(`(?:python3?|perl|bash|sh|Rscript)\s+\{workdir\}/([^\s;|&<>()]+)`)
	wdStdinRef  = regexp.MustCompile(`<\s*\{workdir\}/([^\s;|&<>()]+)`)
	wdOutputRef = regexp.MustCompile(`(?:-o\s+|>>?\s*)\{workdir\}/([^\s;|&<>()]+)`)
)

// warnMissingAssets prints (to stderr) a warning for each {workdir}/<file> a step
// uses as a script or stdin input that is neither a declared `asset` nor produced by
// a step — the common "forgot to add the helper script to `assets`" mistake. It only
// warns (the step will still run and fail if the file is genuinely absent).
func warnMissingAssets(t config.Tool, steps []config.Step) {
	for _, name := range missingAssets(t, steps) {
		fmt.Fprintf(os.Stderr, "warning: tool %s: a step reads {workdir}/%s but it is not a declared `asset` nor produced by a step — add %q to the source's `assets`\n", t.ID(), name, name)
	}
}

// missingAssets returns the base names of {workdir}/<file> inputs (scripts / stdin)
// that are neither declared assets nor produced by a step output. Order-stable, deduped.
func missingAssets(t config.Tool, steps []config.Step) []string {
	have := map[string]bool{}
	for _, a := range t.Assets {
		have[path.Base(a)] = true
	}
	if t.Output != "" {
		have[path.Base(t.Output)] = true
	}
	for _, s := range steps { // files a step produces are available to later steps
		for _, m := range wdOutputRef.FindAllStringSubmatch(s.Run, -1) {
			have[path.Base(m[1])] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{wdScriptRef, wdStdinRef} {
		for _, s := range steps {
			for _, m := range re.FindAllStringSubmatch(s.Run, -1) {
				name := path.Base(m[1])
				if name == "" || name == "." || have[name] || seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// threadsOf is the per-run thread count for {threads} (>=1), e.g. vep --fork.
func threadsOf(t config.Tool) int {
	if t.Threads > 0 {
		return t.Threads
	}
	return 1
}

// replacer builds the host-path template substitutions for a non-container step.
func replacer(t config.Tool, p Params) *strings.Replacer {
	return strings.NewReplacer(
		"{input}", p.Input,
		"{output}", p.Output,
		"{workdir}", p.Workdir,
		"{image}", p.Image,
		"{ref}", p.Ref,
		"{datadir}", p.Datadir,
		"{chrom}", p.Chrom,
		"{threads}", strconv.Itoa(threadsOf(t)),
	)
}

// absDir returns p as an absolute path (resolved against varhub's CWD), or p
// unchanged if that fails. Container binds must be absolute — see containerMapping.
func absDir(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// ctrRoot is the fixed in-container mount root for container steps.
const ctrRoot = "/varhub"

// containerMapping binds each of p's host dirs to a fixed, shallow mountpoint under
// ctrRoot and returns the template replacer (expanding placeholders to those
// in-container paths) together with the matching `-B host:dest` flags. Decoupling
// the in-container paths from the deep host paths means the engine only creates
// shallow /varhub/* mountpoints — which avoids the engine having to recreate a deep
// host path inside a read-only image (the cause of INSTALL.pl "Could not create
// directory" failures) and keeps a registry tool's commands host-independent.
func containerMapping(t config.Tool, p Params) (*strings.Replacer, []string) {
	var binds []string
	bind := func(host, dest string) {
		if host != "" {
			binds = append(binds, "-B", host+":"+dest)
		}
	}

	data := ctrRoot + "/data"
	work := ctrRoot + "/work"
	bind(p.Datadir, data)
	bind(p.Workdir, work)

	// Only bind the reference/input dirs when they exist on the host. A step that
	// doesn't use {ref}/{input} (e.g. tool setup) must not fail because the reference
	// FASTA isn't set up yet; a step that DOES use a missing {ref} still renders the
	// in-container path and fails clearly ("no such file") rather than on a mount hook.
	// Bind host directories by ABSOLUTE path: the container engine resolves a
	// relative -B source against ITS working directory (exec1 sets that to the tool
	// workdir), not varhub's CWD — so a relative {ref}/{input} (e.g. an input VCF
	// given as `sub/dir/in.vcf.gz`) would mount a non-existent path under the workdir.
	// filepath.Abs resolves against varhub's CWD, where the path is actually valid.
	ref := ""
	if p.Ref != "" {
		if dir := absDir(filepath.Dir(p.Ref)); dirExists(dir) {
			bind(dir, ctrRoot+"/ref")
		}
		ref = ctrRoot + "/ref/" + filepath.Base(p.Ref)
	}
	input := ""
	if p.Input != "" {
		if dir := absDir(filepath.Dir(p.Input)); dirExists(dir) {
			bind(dir, ctrRoot+"/in")
		}
		input = ctrRoot + "/in/" + filepath.Base(p.Input)
	}
	// Output is always written under the workdir (filepath.Join(workdir, OutputName)),
	// so it maps under the workdir bind — no extra mount needed.
	output := ""
	if p.Output != "" {
		output = work + "/" + filepath.Base(p.Output)
	}

	repl := strings.NewReplacer(
		"{input}", input,
		"{output}", output,
		"{workdir}", work,
		"{image}", p.Image,
		"{ref}", ref,
		"{datadir}", data,
		"{chrom}", p.Chrom,
		"{threads}", strconv.Itoa(threadsOf(t)),
	)
	return repl, binds
}

func runStep(ctx context.Context, t config.Tool, step config.Step, idx int, p Params) error {
	// Container steps render against fixed /varhub/* mountpoints; host steps use the
	// real host paths. The script always lives in the (host) workdir.
	var repl *strings.Replacer
	var binds []string
	if step.Container {
		repl, binds = containerMapping(t, p)
	} else {
		repl = replacer(t, p)
	}
	run := repl.Replace(step.Run)

	// Write the rendered command to a script so we avoid inline quoting issues
	// (and so the whole pipeline runs inside the container for container steps).
	script := filepath.Join(p.Workdir, fmt.Sprintf("step-%d.sh", idx))
	if err := os.WriteFile(script, []byte("set -euo pipefail\n"+run+"\n"), 0o755); err != nil {
		return err
	}

	var inner []string
	if step.Container {
		if p.Image == "" {
			return fmt.Errorf("container step needs an image (set tool.image)")
		}
		inner = append([]string{t.ContainerEngine(), "exec", "--no-home"}, binds...)
		// The script lives in the host workdir, which is bound at /varhub/work.
		inner = append(inner, p.Image, "bash", ctrRoot+"/work/"+filepath.Base(script))
	} else {
		inner = []string{"bash", script}
	}

	return exec1(ctx, p.Workdir, inner[0], inner[1:]...)
}

// exec1 runs a command, streaming its output to stderr (data flows through
// files, not stdout).
func exec1(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ensureIndex builds a tabix index for the output if one isn't present.
func ensureIndex(path, format string) error {
	if fileExists(path+".tbi") || fileExists(path+".csi") {
		return nil
	}
	opts, err := preset(format)
	if err != nil {
		return err
	}
	return tabix.NewIndexWriter(opts).WriteIndex(path)
}

func preset(format string) (*tabix.WriterOpts, error) {
	switch format {
	case "vcf", "":
		return tabix.NewWriterOpts().VCF(), nil
	case "tab":
		return tabix.NewWriterOpts().Columns(1, 2, 0).Meta('#'), nil
	default:
		return nil, fmt.Errorf("cannot index tool output of format %q (want vcf|tab)", format)
	}
}

func stepLabel(s config.Step, i int) string {
	if s.Name != "" {
		return s.Name
	}
	return strconv.Itoa(i)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
