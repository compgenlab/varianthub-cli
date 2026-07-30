// Package objstore adds S3 as a destination for the source cache.
//
// A cache location is a "locator": either a filesystem path or an s3:// URI.
// The same locator resolution decides where `varhub download` writes and (once
// the read path lands) where `varhub annotate` reads, so there is one concept
// here rather than a parallel set of remote-only paths.
//
// This file is deliberately free of any AWS dependency: locators are parsed and
// composed with string handling alone, so code that only needs to *recognise* a
// remote cache does not pull in an SDK.
package objstore

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Scheme prefixes a remote locator.
const Scheme = "s3://"

// IsRemote reports whether a locator names an object store rather than a
// filesystem path.
func IsRemote(locator string) bool {
	return strings.HasPrefix(locator, Scheme)
}

// Join appends path segments to a locator.
//
// It exists because filepath.Join cannot be used on an s3:// URI: it calls
// Clean, which collapses the "//" in the scheme and yields "s3:/bucket". Local
// locators keep filepath semantics so nothing changes for them.
func Join(locator string, parts ...string) string {
	if !IsRemote(locator) {
		return filepath.Join(append([]string{locator}, parts...)...)
	}
	rest := strings.TrimPrefix(locator, Scheme)
	return Scheme + path.Join(append([]string{rest}, parts...)...)
}

// Base returns the final segment of a locator.
func Base(locator string) string {
	if !IsRemote(locator) {
		return filepath.Base(locator)
	}
	return path.Base(strings.TrimPrefix(locator, Scheme))
}

// Dir returns the locator with its final segment removed.
func Dir(locator string) string {
	if !IsRemote(locator) {
		return filepath.Dir(locator)
	}
	rest := strings.TrimPrefix(locator, Scheme)
	d := path.Dir(rest)
	if d == "." || d == "/" {
		return Scheme + strings.Trim(rest, "/")
	}
	return Scheme + d
}

// Ref is a parsed s3:// locator.
type Ref struct {
	Bucket string
	Key    string
}

// String renders the ref back to an s3:// locator.
func (r Ref) String() string {
	if r.Key == "" {
		return Scheme + r.Bucket
	}
	return Scheme + r.Bucket + "/" + r.Key
}

// Parse splits an s3://bucket/key locator.
//
// A bare s3://bucket is valid and yields an empty key — that is a cache root,
// which callers extend with Join before addressing an object.
func Parse(locator string) (Ref, error) {
	if !IsRemote(locator) {
		return Ref{}, fmt.Errorf("not an s3 locator: %q", locator)
	}
	rest := strings.TrimPrefix(locator, Scheme)
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Ref{}, fmt.Errorf("s3 locator %q has no bucket", locator)
	}
	key = strings.Trim(key, "/")
	return Ref{Bucket: bucket, Key: key}, nil
}
