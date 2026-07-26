package annotate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// toolRunManifest records a saved tool run in a --tool-cache-dir so a later run
// with the same input can reuse the output instead of re-running the tool. The
// input is matched by absolute path + size + mtime (a cheap `make`-style staleness
// check): regenerating the input bumps its mtime, so a stale output is never reused.
type toolRunManifest struct {
	Tool         string `toml:"tool"`
	Version      string `toml:"version"`
	Assembly     string `toml:"assembly"`
	InputPath    string `toml:"input_path"`
	InputSize    int64  `toml:"input_size"`
	InputMtimeNs int64  `toml:"input_mtime_ns"`
	Output       string `toml:"output"` // data file basename within the cache dir
	Format       string `toml:"format"`
	Created      string `toml:"created"` // run timestamp (also in the filenames)
}

// inputStat returns the input's absolute path, size, and mtime (ns) — the identity
// used to match a cached tool output to an input.
func inputStat(inPath string) (abs string, size, mtimeNs int64, err error) {
	fi, err := os.Stat(inPath)
	if err != nil {
		return "", 0, 0, err
	}
	abs, err = filepath.Abs(inPath)
	if err != nil {
		abs = inPath
	}
	return abs, fi.Size(), fi.ModTime().UnixNano(), nil
}

// lookupToolCache returns the path of a saved output in dir whose manifest matches
// this tool (name+version), assembly, and input (path+size+mtime). The newest match
// whose data file and index still exist wins. ok=false when there is no usable match.
func lookupToolCache(dir string, t config.Tool, assembly, inPath string) (string, bool, error) {
	abs, size, mtimeNs, err := inputStat(inPath)
	if err != nil {
		return "", false, err
	}
	manifests, err := filepath.Glob(filepath.Join(dir, "*.run.toml"))
	if err != nil {
		return "", false, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(manifests))) // timestamped names → newest first
	for _, mf := range manifests {
		var m toolRunManifest
		if _, err := toml.DecodeFile(mf, &m); err != nil {
			continue // ignore an unreadable/foreign manifest
		}
		if m.Tool != t.Name || m.Version != t.Version || m.Assembly != assembly {
			continue
		}
		if m.InputPath != abs || m.InputSize != size || m.InputMtimeNs != mtimeNs {
			continue
		}
		data := filepath.Join(dir, m.Output)
		idxExt, err := indexExt(data)
		if err != nil {
			continue // output or its index is gone — not usable
		}
		_ = idxExt
		if a, err := filepath.Abs(data); err == nil {
			return a, true, nil
		}
		return data, true, nil
	}
	return "", false, nil
}

// storeToolCache saves a freshly-produced tool output (outFile) into dir for reuse:
// the bgzipped data file (+ its index), a run manifest recording the input identity,
// and a drop-in `[[sources]]` stub. Filenames embed a run timestamp so runs never
// collide, and files are created O_EXCL (never overwritten).
func storeToolCache(dir string, src config.Source, assembly, inPath, outFile string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, size, mtimeNs, err := inputStat(inPath)
	if err != nil {
		return err
	}
	format := src.Format
	if format == "" {
		format = "vcf"
	}
	// Sub-second precision so distinct saves never collide (even within one second),
	// and the timestamp still sorts lexically = chronologically for newest-first reuse.
	stamp := time.Now().Format("2006-01-02T15-04-05.000000000")
	base := filepath.Join(dir, fmt.Sprintf("%s-%s.%s", src.Name, src.Version, stamp))
	dataPath := base + "." + format + ".gz"

	if err := copyExcl(outFile, dataPath); err != nil {
		return fmt.Errorf("save tool %s output: %w", src.ID(), err)
	}
	idxExt, err := indexExt(outFile)
	if err != nil {
		return fmt.Errorf("save tool %s output: %w", src.ID(), err)
	}
	if err := copyExcl(outFile+idxExt, dataPath+idxExt); err != nil {
		return fmt.Errorf("save tool %s index: %w", src.ID(), err)
	}
	if err := writeSourceStub(base+".toml", src, dataPath, format); err != nil {
		return fmt.Errorf("save tool %s source stub: %w", src.ID(), err)
	}

	m := toolRunManifest{
		Tool: src.Name, Version: src.Version, Assembly: assembly,
		InputPath: abs, InputSize: size, InputMtimeNs: mtimeNs,
		Output: filepath.Base(dataPath), Format: format, Created: stamp,
	}
	return writeTOMLExcl(base+".run.toml", m)
}

// writeSourceStub writes a drop-in `[[sources]]` fragment: a type="" source at
// dataPath (absolute, so it resolves regardless of data_dir) carrying the tool's
// own annotations, so the saved output can be referenced as a normal static source.
func writeSourceStub(path string, src config.Source, dataPath, format string) error {
	abs, err := filepath.Abs(dataPath)
	if err != nil {
		abs = dataPath
	}
	stub := &config.Snapshot{Sources: []config.Source{{
		Name: src.Name, Version: src.Version, Format: format,
		LocalPath: abs, RefCol: src.RefCol, AltCol: src.AltCol,
		Annotations: src.Annotations,
	}}}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite %s", path)
	}
	return config.WriteFragment(path, stub)
}

// writeTOMLExcl encodes v to a new TOML file, failing if it already exists.
func writeTOMLExcl(path string, v any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(v)
}

// indexExt returns the extension of the index sitting beside a bgzipped file
// (".tbi" or ".csi"), or an error if neither is present.
func indexExt(path string) (string, error) {
	for _, ext := range []string{".tbi", ".csi"} {
		if _, err := os.Stat(path + ext); err == nil {
			return ext, nil
		}
	}
	return "", fmt.Errorf("no .tbi/.csi index beside %s", path)
}

// copyExcl copies src → dst, failing (never overwriting) if dst already exists.
func copyExcl(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite %s", dst)
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
