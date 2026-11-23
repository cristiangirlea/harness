package pluginproto

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTheCopyMatchesTheContractsModuleByteForByte(t *testing.T) {
	contracts := filepath.Join("..", "..", "contracts", "pluginproto")
	checkCopy := func(source, target string, compareBytes bool) error {
		return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "copies_test.go" {
				if compareBytes {
					return fmt.Errorf("%s: exists in the contracts package", rel)
				}
				return nil
			}
			if compareBytes && !entry.Type().IsRegular() {
				return nil
			}
			other := filepath.Join(target, filepath.FromSlash(rel))
			info, err := os.Lstat(other)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s: counterpart is not a regular file", rel)
			}
			if !compareBytes {
				return nil
			}
			want, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			got, err := os.ReadFile(other)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("%s: file contents differ", rel)
			}
			return nil
		})
	}
	if err := checkCopy(contracts, ".", true); err != nil {
		t.Fatal(err)
	}
	if err := checkCopy(".", contracts, false); err != nil {
		t.Fatal(err)
	}
}

func TestEveryExampleInThePluginsPageIsAGoldenFixture(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugins.md"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]bool{}
	for _, dir := range []string{"host", "plugin"} {
		files, err := filepath.Glob(filepath.Join("testdata", "v[12]", dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			line, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			fixtures[strings.TrimSuffix(string(line), "\n")] = true
		}
	}
	text := strings.ReplaceAll(string(page), "\r\n", "\n")
	examples := regexp.MustCompile("(?s)```json\n(.*?)\n```").FindAllStringSubmatch(text, -1)
	if len(examples) == 0 {
		t.Fatal("the page has no json examples")
	}
	for _, example := range examples {
		if !fixtures[example[1]] {
			t.Errorf("no fixture holds this example byte for byte: %s", example[1])
		}
	}
}
