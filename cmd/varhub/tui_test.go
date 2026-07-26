package main

import (
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

func TestMissingSourceFields(t *testing.T) {
	cases := []struct {
		name string
		src  config.Source
		want string
	}{
		{"complete url", config.Source{Name: "x", Version: "1", URL: "https://x"}, "✓ complete"},
		{"complete localpath", config.Source{Name: "x", Version: "1", LocalPath: "/x"}, "✓ complete"},
		{"missing all", config.Source{}, "⚠ missing: name, version, url/localpath"},
		{"missing loc", config.Source{Name: "x", Version: "1"}, "⚠ missing: url/localpath"},
		{"builtin empty", config.Source{Type: "builtin"}, "⚠ missing: annotations"},
		{"builtin ok", config.Source{Type: "builtin", Annotations: []config.Annotation{{Builtin: "tstv"}}}, "✓ complete"},
	}
	for _, c := range cases {
		if got := badge(missingSourceFields(c.src)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMissingToolFields(t *testing.T) {
	if got := badge(missingToolFields(config.Tool{Name: "vep", Version: "1", Steps: []config.Step{{Run: "x"}}})); got != "✓ complete" {
		t.Errorf("complete tool: %q", got)
	}
	if got := badge(missingToolFields(config.Tool{})); got != "⚠ missing: name, version, steps" {
		t.Errorf("empty tool: %q", got)
	}
}

func TestTypeIndex(t *testing.T) {
	if typeIndex("") != 0 || config.AnnotationTypes[typeIndex("")] != "categorical" {
		t.Error("empty type should default to categorical (index 0)")
	}
	if config.AnnotationTypes[typeIndex("numeric")] != "numeric" {
		t.Error("numeric index wrong")
	}
}
