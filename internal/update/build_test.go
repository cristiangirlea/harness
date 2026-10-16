package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildRefRunsGoInstallIntoTheGivenGOBIN(t *testing.T) {
	dir := t.TempDir()
	var ran *exec.Cmd
	b := Builder{
		LookPath: func(name string) (string, error) { return "/usr/local/go/bin/" + name, nil },
		Run: func(cmd *exec.Cmd) error {
			ran = cmd
			return os.WriteFile(filepath.Join(dir, exe("hrn")), []byte("built"), 0o755)
		},
	}
	t.Setenv("GOFLAGS", "-mod=vendor")
	t.Setenv("GOPROXY", "https://proxy.example")

	path, err := b.BuildRef(context.Background(), "main", dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, exe("hrn")) {
		t.Errorf("path %q", path)
	}
	if ran == nil {
		t.Fatal("go was not run")
	}
	if want := []string{"/usr/local/go/bin/go", "install", "github.com/cristiangirlea/harness/cmd/hrn@main"}; !reflect.DeepEqual(ran.Args, want) {
		t.Errorf("args %q; want %q", ran.Args, want)
	}
	env := strings.Join(ran.Env, "\n")
	if !strings.Contains(env, "GOBIN="+dir) {
		t.Errorf("env lacks GOBIN=%s", dir)
	}
	if strings.Contains(env, "GOFLAGS=-mod=vendor") {
		t.Error("GOFLAGS was passed through")
	}
	if !strings.Contains(env, "GOPROXY=https://proxy.example") {
		t.Error("GOPROXY was not left as configured")
	}
}

func TestBuildRefSaysWhenGoIsMissing(t *testing.T) {
	b := Builder{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	_, err := b.BuildRef(context.Background(), "main", t.TempDir())
	if err == nil || err.Error() != "hrn update --ref needs Go on PATH; use a release instead" {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRefReportsAFailedInstall(t *testing.T) {
	b := Builder{
		LookPath: func(name string) (string, error) { return name, nil },
		Run:      func(*exec.Cmd) error { return errors.New("exit status 1") },
	}
	_, err := b.BuildRef(context.Background(), "nosuchref", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nosuchref") {
		t.Fatalf("err = %v; want the ref named", err)
	}
}

func TestBuildRefRefusesAnEmptyRef(t *testing.T) {
	if _, err := (Builder{}).BuildRef(context.Background(), "", t.TempDir()); err == nil {
		t.Fatal("an empty ref built")
	}
}
