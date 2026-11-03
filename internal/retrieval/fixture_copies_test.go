package retrieval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWireFixturesAreByteIdenticalOnBothSides(t *testing.T) {
	pairs := [][2]string{{"testdata/wire", "../../server/platform/testdata/knowledge"}, {"../remote/testdata/server", "../../server/platform/testdata/remote"}}
	for _, pair := range pairs {
		left, err := os.ReadDir(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadDir(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != len(right) {
			t.Fatalf("fixture counts differ: %s and %s", pair[0], pair[1])
		}
		for i, entry := range left {
			if entry.IsDir() || right[i].IsDir() || entry.Name() != right[i].Name() {
				t.Fatalf("fixture names differ: %s and %s", entry.Name(), right[i].Name())
			}
			a, err := os.ReadFile(filepath.Join(pair[0], entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(pair[1], entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatalf("fixture differs: %s", entry.Name())
			}
		}
	}
}
