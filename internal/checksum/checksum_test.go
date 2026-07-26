package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestValidateSpec(t *testing.T) {
	// "" means no checksum configured → valid.
	cases := map[string]bool{
		"": true,
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855": true,
		"sha1:da39a3ee5e6b4b0d3255bfef95601890afd80709":                         true,
		"md5:d41d8cd98f00b204e9800998ecf8427e":                                   true,
		"SHA256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855": true, // case-insensitive
		"sha256:nothex": false,
		"sha256:dead":   false, // wrong length
		"crc32:abcd":    false, // unsupported algo
		"deadbeef":      false, // missing algo:
	}
	for spec, ok := range cases {
		err := ValidateSpec(spec)
		if (err == nil) != ok {
			t.Errorf("ValidateSpec(%q) err=%v, want ok=%v", spec, err, ok)
		}
	}
}

func TestVerifierStreaming(t *testing.T) {
	const data = "hello varhub"
	// sha256("hello varhub")
	good := "sha256:" + sha256Hex(data)

	v, err := New(good)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(v, strings.NewReader(data))
	if err := v.Check(); err != nil {
		t.Errorf("correct digest should pass: %v", err)
	}

	bad, _ := New("sha256:" + strings.Repeat("0", 64))
	io.Copy(bad, strings.NewReader(data))
	if err := bad.Check(); err == nil {
		t.Error("wrong digest should fail")
	}

	// Nil verifier (empty spec) is a no-op that passes.
	none, err := New("")
	if err != nil || none != nil {
		t.Fatalf("empty spec: v=%v err=%v, want nil/nil", none, err)
	}
	if _, err := none.Write([]byte(data)); err != nil {
		t.Errorf("nil verifier Write: %v", err)
	}
	if err := none.Check(); err != nil {
		t.Errorf("nil verifier Check should pass: %v", err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
