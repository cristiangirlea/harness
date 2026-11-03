package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARecordWithoutASetupStillLoads(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := "20261014-120000-abcdef"
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"` + id + `","backend":"api","task":{"prompt":"p","dir":"."},"result":{"status":"ok"}}`
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(id)
	if err != nil || rec.Setup != nil || rec.Task.Prompt != "p" {
		t.Fatalf("old record = %+v, error = %v", rec, err)
	}
}
