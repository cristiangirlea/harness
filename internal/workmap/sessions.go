package workmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var sessionActiveAge = 15 * time.Minute
var sessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type sessionLine struct {
	Cwd       string `json:"cwd"`
	Branch    string `json:"gitBranch"`
	ID        string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
}
type sessionFile struct {
	path, id string
	modified time.Time
}

func sessionInside(dir, root string) bool {
	dir, root = canonical(dir), canonical(root)
	if runtime.GOOS == "windows" {
		dir, root = strings.ToLower(dir), strings.ToLower(root)
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sessionSources(roots map[string]string, worktrees map[string]map[string]string, defaults map[string]string, pieces map[string]*Piece) error {
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(home, ".claude")
	}
	projects := filepath.Join(home, "projects")
	dirs, err := entries(projects)
	if err != nil {
		return err
	}
	var files []sessionFile
	var errs []error
	now := time.Now()
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		path := filepath.Join(projects, dir.Name())
		list, e := entries(path)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		for _, entry := range list {
			id, ok := strings.CutSuffix(entry.Name(), ".jsonl")
			if !ok || !sessionID.MatchString(id) || entry.Type()&os.ModeType != 0 {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				errs = append(errs, e)
				continue
			}
			if now.Sub(info.ModTime()) <= 24*time.Hour {
				files = append(files, sessionFile{filepath.Join(path, entry.Name()), id, info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modified.Equal(files[j].modified) {
			return files[i].path < files[j].path
		}
		return files[i].modified.After(files[j].modified)
	})
	if len(files) > 200 {
		files = files[:200]
	}
	for _, file := range files {
		f, e := os.Open(file.path)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		size, e := f.Seek(0, io.SeekEnd)
		if e == nil && size > 65536 {
			_, e = f.Seek(-65536, io.SeekEnd)
		} else if e == nil {
			_, e = f.Seek(0, io.SeekStart)
		}
		var data []byte
		if e == nil {
			data, e = io.ReadAll(io.LimitReader(f, 65536))
		}
		f.Close()
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if size > 65536 {
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				data = data[i+1:]
			} else {
				continue
			}
		}
		lines := bytes.Split(data, []byte{'\n'})
		var record sessionLine
		for i := len(lines) - 1; i >= 0; i-- {
			var line sessionLine
			if json.Unmarshal(lines[i], &line) == nil && line.Cwd != "" && line.Branch != "" {
				record = line
				break
			}
		}
		if record.Cwd == "" {
			continue
		}
		root := ""
		longest := -1
		for r := range roots {
			paths := []string{r}
			for path := range worktrees[r] {
				paths = append(paths, path)
			}
			for _, path := range paths {
				if len(path) > longest && sessionInside(record.Cwd, path) {
					root, longest = r, len(path)
				}
			}
		}
		if root == "" || record.Branch == defaults[root] {
			continue
		}
		key := root + "\x00branch:" + record.Branch
		p := pieces[key]
		if p == nil {
			p = &Piece{Repo: root, Key: "branch:" + record.Branch, Title: record.Branch, Stage: Branch, Links: Links{Branch: record.Branch}}
			pieces[key] = p
		}
		p.Links.Sessions = append(p.Links.Sessions, file.id)
		if now.Sub(file.modified) <= sessionActiveAge {
			p.ActiveSession = true
			if rank(p.Stage) < rank(Agent) {
				p.Stage = Agent
			}
		}
	}
	return errors.Join(errs...)
}
