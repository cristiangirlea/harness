package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	oldLine = "hrn 0.2.0 (go1.26.4, a0e5d2f12345)"
	newLine = "hrn 0.2.0 (go1.26.4, b1f6e3a23456)"
	stamp   = time.Date(2026, 10, 15, 9, 7, 0, 0, time.Local)
)

func parse(t *testing.T, line string) Version {
	t.Helper()
	v, err := ParseVersionLine(line)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBackupNameCarriesTheOldBuildAndTheMinute(t *testing.T) {
	v := parse(t, oldLine)
	if got := BackupName(filepath.Join("C:", "bin", "hrn.exe"), v, stamp); got != "hrn-a0e5d2f12345-20261015-0907.prev.exe" {
		t.Errorf("got %q", got)
	}
	if got := BackupName("/usr/local/bin/hrn", v, stamp); got != "hrn-a0e5d2f12345-20261015-0907.prev" {
		t.Errorf("got %q", got)
	}
	if got := BackupName("/usr/local/bin/hrn", Version{}, stamp); got != "hrn-unknown-20261015-0907.prev" {
		t.Errorf("an unreadable version: got %q", got)
	}
}

// installation sets up a folder with one installed fake hrn and a candidate
// elsewhere, and a version function that knows both.
func installation(t *testing.T) (target, candidate string, version VersionFunc) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, exe("hrn"))
	fakeHrn(t, target, "old")
	candidate = filepath.Join(t.TempDir(), exe("hrn-candidate"))
	fakeHrn(t, candidate, "new build")
	return target, candidate, fakeVersion(t, map[string]string{target: oldLine, candidate: newLine})
}

func TestSwapRenamesTheOldBinaryAsideAndCopiesTheCandidateIn(t *testing.T) {
	target, candidate, version := installation(t)
	ctx := context.Background()

	backup, err := Swap(ctx, target, candidate, parse(t, oldLine), parse(t, newLine), stamp, version)
	if err != nil {
		t.Fatal(err)
	}
	wantBackup := filepath.Join(filepath.Dir(target), "hrn-a0e5d2f12345-20261015-0907.prev"+filepath.Ext(target))
	if backup != wantBackup {
		t.Errorf("backup %q; want %q", backup, wantBackup)
	}
	if !sameFile(t, target, candidate) {
		t.Error("the target is not the candidate's bytes")
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Errorf("backup: %v", err)
	}
	if info, _ := os.Stat(target); info != nil && !isWindows() && info.Mode().Perm() != 0o755 {
		t.Errorf("target mode %v; want 0755", info.Mode().Perm())
	}
	if got, err := version(ctx, target); err != nil || got.Line != newLine {
		t.Errorf("the target now says %q, %v; want %q", got.Line, err, newLine)
	}
}

func TestSwapWorksWhileTheOldBinaryRuns(t *testing.T) {
	// Windows refuses to overwrite or delete a running executable but allows
	// a rename, which is what the whole update relies on.
	target, candidate, version := installation(t)
	cmd := exec.Command(target, "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), "HRN_UPDATE_HELPER_SLEEP=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()

	if _, err := Swap(context.Background(), target, candidate, parse(t, oldLine), parse(t, newLine), stamp, version); err != nil {
		t.Fatalf("swap under a running process: %v", err)
	}
	if !sameFile(t, target, candidate) {
		t.Error("the target is not the candidate's bytes")
	}
}

func TestSwapPutsTheBackupBackWhenTheNewBinaryDoesNotAnswer(t *testing.T) {
	target, candidate, _ := installation(t)
	// The candidate is known to the version function, the copy at the target
	// path is answered with the wrong line: the verify step must catch it.
	calls := 0
	version := func(ctx context.Context, path string) (Version, error) {
		calls++
		if path == target {
			return parse(t, "hrn 0.2.0 (go1.26.4, cafe12345678)"), nil
		}
		return parse(t, newLine), nil
	}
	before, _ := os.ReadFile(target)

	_, err := Swap(context.Background(), target, candidate, parse(t, oldLine), parse(t, newLine), stamp, version)
	if err == nil || !strings.Contains(err.Error(), "cafe12345678") {
		t.Fatalf("err = %v; want the verify failure naming what the new file said", err)
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Error("the old binary was not put back")
	}
	if got := names(t, filepath.Dir(target)); len(got) != 1 {
		t.Errorf("files beside the target after the restore: %v; want only the target", got)
	}
	if calls == 0 {
		t.Error("the new file was never run with version")
	}
}

func TestSwapRefusesWhenTheTargetFolderIsNotWritable(t *testing.T) {
	target, candidate, version := installation(t)
	missing := filepath.Join(filepath.Dir(target), "nowhere", exe("hrn"))
	_, err := Swap(context.Background(), missing, candidate, parse(t, oldLine), parse(t, newLine), stamp, version)
	if err == nil {
		t.Fatal("a missing target swapped")
	}
	if !sameFile(t, candidate, candidate) || len(names(t, filepath.Dir(target))) != 1 {
		t.Error("the refusal left files behind")
	}
}

func TestBackupsListsTheNewestFirst(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, exe("hrn"))
	ext := filepath.Ext(target)
	for _, n := range []string{
		"hrn-a0e5d2f12345-20261014-2200.prev" + ext,
		"hrn-v0.1.0-20261015-0907.prev" + ext,
		"hrn-unknown-20261001-0000.prev" + ext,
		"hrn-b1f6e3a23456-20261015-0905.rolledback" + ext,
		"notes.txt",
	} {
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644)
	}
	got, err := Backups(target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hrn-v0.1.0-20261015-0907.prev" + ext, "hrn-a0e5d2f12345-20261014-2200.prev" + ext, "hrn-unknown-20261001-0000.prev" + ext}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Errorf("[%d] %q; want %q", i, filepath.Base(got[i]), want[i])
		}
	}
}

func TestRollbackSwapsTheNewestBackupBackIn(t *testing.T) {
	target, candidate, version := installation(t)
	ctx := context.Background()
	backup, err := Swap(ctx, target, candidate, parse(t, oldLine), parse(t, newLine), stamp, version)
	if err != nil {
		t.Fatal(err)
	}
	later := stamp.Add(90 * time.Minute)

	restored, err := Rollback(ctx, target, later, version)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Line != oldLine {
		t.Errorf("restored %q; want %q", restored.Line, oldLine)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the backup still exists beside the target: %v", err)
	}
	if got, err := version(ctx, target); err != nil || got.Line != oldLine {
		t.Errorf("the target says %q, %v; want the old line", got.Line, err)
	}
	rolled := "hrn-b1f6e3a23456-20261015-1037.rolledback" + filepath.Ext(target)
	found := false
	for _, n := range names(t, filepath.Dir(target)) {
		if n == rolled {
			found = true
		}
	}
	if !found {
		t.Errorf("files: %v; want %s", names(t, filepath.Dir(target)), rolled)
	}
}

func TestRollbackRefusesWithoutABackup(t *testing.T) {
	target, _, version := installation(t)
	_, err := Rollback(context.Background(), target, stamp, version)
	if !errors.Is(err, ErrNoBackup) {
		t.Fatalf("err = %v; want ErrNoBackup", err)
	}
	if got := names(t, filepath.Dir(target)); len(got) != 1 {
		t.Errorf("files: %v; want the target untouched", got)
	}
}

func TestRollbackUndoesItselfWhenTheBackupDoesNotAnswer(t *testing.T) {
	target, _, version := installation(t)
	dir := filepath.Dir(target)
	backup := filepath.Join(dir, "hrn-unknown-20261014-2200.prev"+filepath.Ext(target))
	os.WriteFile(backup, []byte("not a program"), 0o755)
	before, _ := os.ReadFile(target)

	_, err := Rollback(context.Background(), target, stamp, version)
	if err == nil {
		t.Fatal("a backup that is not hrn was rolled in")
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Error("the current binary was not put back")
	}
	if b, err := os.ReadFile(backup); err != nil || string(b) != "not a program" {
		t.Errorf("the backup file changed: %q %v", b, err)
	}
}
