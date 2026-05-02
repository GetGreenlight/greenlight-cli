//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestScrubBasic(t *testing.T) {
	secret := []byte("supersecrettoken")
	forms := encodingsOf(secret)
	in := bytes.NewBufferString("before " + string(secret) + " after\n")
	var out bytes.Buffer
	if err := scrub(in, &out, forms); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), string(secret)) {
		t.Fatalf("plaintext leaked: %q", out.String())
	}
	if !strings.Contains(out.String(), "***") {
		t.Fatalf("redaction missing: %q", out.String())
	}
}

func TestScrubEncodedForms(t *testing.T) {
	secret := []byte("verysecrettoken1234")
	cases := []string{
		hex.EncodeToString(secret),
		strings.ToUpper(hex.EncodeToString(secret)),
		base64.StdEncoding.EncodeToString(secret),
		base64.RawStdEncoding.EncodeToString(secret),
		base64.URLEncoding.EncodeToString(secret),
		base64.RawURLEncoding.EncodeToString(secret),
	}
	forms := encodingsOf(secret)
	for _, form := range cases {
		in := strings.NewReader("leaked: " + form + " end")
		var out bytes.Buffer
		if err := scrub(in, &out, forms); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), form) {
			t.Fatalf("encoded form leaked %q in %q", form, out.String())
		}
	}
}

func TestScrubChunkBoundary(t *testing.T) {
	secret := []byte("chunkBoundary12345")
	forms := encodingsOf(secret)
	full := "AAA" + string(secret) + "BBB"

	// Use a tiny reader to force chunk boundaries inside the secret.
	r := &chunkReader{data: []byte(full), chunk: 5}
	var out bytes.Buffer
	if err := scrub(r, &out, forms); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), string(secret)) {
		t.Fatalf("secret leaked across chunk boundary: %q", out.String())
	}
	if !strings.HasPrefix(out.String(), "AAA") || !strings.HasSuffix(out.String(), "BBB") {
		t.Fatalf("surrounding text mangled: %q", out.String())
	}
}

func TestScrubMultipleSecretsPrefixCollision(t *testing.T) {
	// One secret is a prefix of another.
	long := []byte("alphabravo01234567")
	short := []byte("alphabravo")
	forms := dedupeForms(append(encodingsOf(long), encodingsOf(short)...))
	in := strings.NewReader(string(long) + " and " + string(short))
	var out bytes.Buffer
	if err := scrub(in, &out, forms); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), string(short)) {
		t.Fatalf("short leaked: %q", out.String())
	}
	if strings.Contains(out.String(), string(long)) {
		t.Fatalf("long leaked: %q", out.String())
	}
}

func TestScrubEmptyForms(t *testing.T) {
	in := strings.NewReader("nothing to scrub here")
	var out bytes.Buffer
	if err := scrub(in, &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "nothing to scrub here" {
		t.Fatalf("passthrough mismatch: %q", out.String())
	}
}

// chunkReader returns `chunk` bytes per Read call.
type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.chunk
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}
