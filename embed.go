package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extractEmbeddedLib decompresses the embedded interpose library,
// writes it to a temp file, and returns its path. The caller should
// defer os.Remove on the returned path.
func extractEmbeddedLib() string {
	if len(embeddedLib) == 0 {
		return ""
	}

	r, err := gzip.NewReader(bytes.NewReader(embeddedLib))
	if err != nil {
		return ""
	}
	defer r.Close()
	decoded, err := io.ReadAll(r)
	if err != nil || len(decoded) == 0 {
		return ""
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return ""
	}
	p := filepath.Join(os.TempDir(), fmt.Sprintf(".gl-%s", hex.EncodeToString(rnd[:])))
	if err := os.WriteFile(p, decoded, 0700); err != nil {
		return ""
	}
	return p
}
