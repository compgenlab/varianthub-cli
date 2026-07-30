package objstore

import "testing"

func TestIsRemote(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"s3://bucket/prefix", true},
		{"s3://bucket", true},
		{"/var/cache/varhub", false},
		{"cache", false},
		{"", false},
		{"s3:/bucket", false}, // one slash is not the scheme
	} {
		if got := IsRemote(tc.in); got != tc.want {
			t.Errorf("IsRemote(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Join must not go through filepath.Clean for remote locators: Clean collapses
// the scheme's "//" and silently produces "s3:/bucket", which then addresses a
// relative directory instead of a bucket.
func TestJoinKeepsScheme(t *testing.T) {
	got := Join("s3://bucket/prefix", "clinvar", "2026-01", "clinvar.vcf.gz")
	want := "s3://bucket/prefix/clinvar/2026-01/clinvar.vcf.gz"
	if got != want {
		t.Errorf("Join = %q, want %q", got, want)
	}
	if got := Join("s3://bucket", "a"); got != "s3://bucket/a" {
		t.Errorf("Join onto bare bucket = %q", got)
	}
	if got := Join("/var/cache", "a", "b"); got != "/var/cache/a/b" {
		t.Errorf("local Join = %q", got)
	}
}

func TestBaseAndDir(t *testing.T) {
	loc := "s3://bucket/prefix/clinvar/2026-01/clinvar.vcf.gz"
	if got := Base(loc); got != "clinvar.vcf.gz" {
		t.Errorf("Base = %q", got)
	}
	if got := Dir(loc); got != "s3://bucket/prefix/clinvar/2026-01" {
		t.Errorf("Dir = %q", got)
	}
	if got := Base("/var/cache/x.gz"); got != "x.gz" {
		t.Errorf("local Base = %q", got)
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("s3://bucket/prefix/obj.gz")
	if err != nil {
		t.Fatal(err)
	}
	if r.Bucket != "bucket" || r.Key != "prefix/obj.gz" {
		t.Errorf("Parse = %+v", r)
	}
	if got := r.String(); got != "s3://bucket/prefix/obj.gz" {
		t.Errorf("round trip = %q", got)
	}

	// A bare bucket is a valid cache root; callers Join onto it.
	r, err = Parse("s3://bucket")
	if err != nil {
		t.Fatal(err)
	}
	if r.Bucket != "bucket" || r.Key != "" {
		t.Errorf("bare bucket = %+v", r)
	}

	for _, bad := range []string{"/local/path", "s3://", "s3:///key"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}
