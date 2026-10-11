//go:build windows

package tools

import (
	"io/fs"
	"testing"
	"time"
)

type fakeInfo fs.FileMode

func (m fakeInfo) Name() string       { return "" }
func (m fakeInfo) Size() int64        { return 0 }
func (m fakeInfo) Mode() fs.FileMode  { return fs.FileMode(m) }
func (m fakeInfo) ModTime() time.Time { return time.Time{} }
func (m fakeInfo) IsDir() bool        { return fs.FileMode(m).IsDir() }
func (m fakeInfo) Sys() any           { return nil }

// fakeEntry is one path of a fake file system: its type, where it leads
// ("" when Readlink refuses it), or the error Lstat gives for it.
type fakeEntry struct {
	mode   fs.FileMode
	target string
	err    error
}

// fakeFS answers Lstat and Readlink from a table; a path not in it does not
// exist.
func fakeFS(entries map[string]fakeEntry) (func(string) (fs.FileInfo, error), func(string) (string, error)) {
	lstat := func(p string) (fs.FileInfo, error) {
		e, ok := entries[p]
		switch {
		case !ok:
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
		case e.err != nil:
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: e.err}
		}
		return fakeInfo(e.mode), nil
	}
	readlink := func(p string) (string, error) {
		if e, ok := entries[p]; ok && e.target != "" {
			return e.target, nil
		}
		return "", &fs.PathError{Op: "readlink", Path: p, Err: fs.ErrInvalid}
	}
	return lstat, readlink
}

func TestThroughLinkFollowsTheLinksEvalSymlinksLeaves(t *testing.T) {
	lstat, readlink := fakeFS(map[string]fakeEntry{
		`C:\ws`:          {mode: fs.ModeDir},
		`C:\ws\plain`:    {mode: fs.ModeDir},
		`C:\ws\j`:        {mode: fs.ModeIrregular, target: `D:\elsewhere`},
		`C:\ws\s`:        {mode: fs.ModeSymlink, target: `C:\ws\out\sub`},
		`C:\ws\dangling`: {mode: fs.ModeSymlink, target: `C:\outside\new.txt`},
		`C:\ws\rel`:      {mode: fs.ModeSymlink, target: `..\other`},
		`C:\ws\rooted`:   {mode: fs.ModeSymlink, target: `\top`},
		`C:\ws\onedrive`: {mode: fs.ModeIrregular},
		`C:\ws\folder`:   {mode: fs.ModeDir | fs.ModeIrregular},
		`C:\ws\drive`:    {mode: fs.ModeSymlink, target: `D:x`},
		`C:\ws\unread`:   {mode: fs.ModeSymlink},
		`C:\ws\denied`:   {err: fs.ErrPermission},
		`C:\ws\nt`:       {mode: fs.ModeIrregular, target: `\Device\HarddiskVolume3\x`},
	})
	cases := []struct {
		path, want string
		ok         bool
	}{
		// A junction, which EvalSymlinks does not follow.
		{`C:\ws\j\x`, `D:\elsewhere\x`, true},
		// A symbolic link EvalSymlinks gave up on: its target goes on past a
		// junction, or does not exist yet.
		{`C:\ws\s\deeper.txt`, `C:\ws\out\sub\deeper.txt`, true},
		{`C:\ws\dangling`, `C:\outside\new.txt`, true},
		// Targets relative to the link's folder, or to its volume's root.
		{`C:\ws\rel\a`, `C:\other\a`, true},
		{`C:\ws\rooted\a`, `C:\top\a`, true},
		// No link: a plain path, one that does not exist yet, and OneDrive's
		// reparse points, which are not links.
		{`C:\ws\plain\x`, "", false},
		{`C:\ws\onedrive`, "", false},
		{`C:\ws\folder\x`, "", false},
		// What cannot be judged is refused ("", true) rather than judged by
		// its name: a reparse point gone on past that is not a link Readlink
		// reads, a link it cannot read, a target relative to another drive's
		// working directory, and an Lstat error other than "not there".
		{`C:\ws\onedrive\x`, "", true},
		{`C:\ws\unread\x`, "", true},
		{`C:\ws\unread`, "", true},
		{`C:\ws\drive\a`, "", true},
		{`C:\ws\denied\x`, "", true},
		// A mount point's target is always absolute; one that is not was
		// made by hand (an NT name Readlink passes on as it is).
		{`C:\ws\nt\y`, "", true},
	}
	for _, c := range cases {
		got, ok := throughLinkWith(c.path, lstat, readlink)
		if got != c.want || ok != c.ok {
			t.Errorf("throughLink(%s) = %q, %v; want %q, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}
