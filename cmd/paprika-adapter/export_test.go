package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestWriteTarCarriesTheWholeExport(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "paprika-export-X")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"recipes.json": "[1]", "manifest.json": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := writeTar(&buf, parent); err != nil {
		t.Fatal(err)
	}
	var files []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			files = append(files, h.Name+"="+string(body))
		}
	}
	sort.Strings(files)
	if got := strings.Join(files, " "); got != "paprika-export-X/manifest.json={} paprika-export-X/recipes.json=[1]" {
		t.Fatalf("tar holds %s", got)
	}
}
