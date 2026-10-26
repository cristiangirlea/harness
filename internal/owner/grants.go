package owner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Grant struct {
	Viewer  string `json:"viewer"`
	Owner   string `json:"owner,omitempty"`
	Project string `json:"project"`
}

// ReadGrants rejects the entire file so a partial configuration never widens access.
func ReadGrants(path string) ([]Grant, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var grants []Grant
	if err := json.Unmarshal(data, &grants); err != nil {
		return nil, err
	}
	if grants == nil {
		return nil, errors.New("expected a grants list")
	}
	for i, g := range grants {
		if !Valid(g.Viewer) || !(strings.HasPrefix(g.Viewer, "email:") || strings.HasPrefix(g.Viewer, "user:")) || g.Owner != "" && !Valid(g.Owner) || !filepath.IsAbs(g.Project) {
			return nil, fmt.Errorf("invalid grant at index %d", i)
		}
		grants[i].Project = filepath.Clean(g.Project)
	}
	return grants, nil
}

// CanReadRecord is the read decision for records with a project directory.
func (v Viewer) CanReadRecord(recorded, dir string) bool {
	return v.ReadRecord("", recorded, dir)
}

// ReadRecord reports the same decision to an optional boundary audit hook.
func (v Viewer) ReadRecord(id, recorded, dir string) bool {
	basis := "none"
	switch {
	case v.Admin:
		basis = "admin"
	case v.ID != "" && Of(recorded) == v.ID:
		basis = "owner"
	case v.ID != "" && len(v.Grants) > 0 && filepath.IsAbs(dir):
		// The repository lookup costs a stat per parent, so only a grant check pays for it.
		repo := RepositoryDir(dir)
		for _, g := range v.Grants {
			if g.Viewer == v.ID && Of(g.Owner) == Of(recorded) && within(g.Project, repo) {
				basis = "grant:" + filepath.Clean(g.Project)
				break
			}
		}
	}
	if id != "" && v.OnRead != nil {
		v.OnRead(id, basis)
	}
	return basis != "none"
}

func within(project, dir string) bool {
	if !filepath.IsAbs(project) {
		return false
	}
	project, dir = filepath.Clean(project), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		project, dir = strings.ToLower(project), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(project, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// RepositoryDir finds a repository without starting git; unavailable directories keep their recorded path.
func RepositoryDir(dir string) string {
	if !filepath.IsAbs(dir) {
		return dir
	}
	dir = filepath.Clean(dir)
	for current := dir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		if filepath.Dir(current) == current {
			return dir
		}
	}
}
