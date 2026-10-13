package platform

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var workspaceID = regexp.MustCompile("^[tpj]_[0-9a-f]{32}$")

type workspaceManifest struct {
	Version       int    `json:"version"`
	TenantID      string `json:"tenant_id"`
	ProjectID     string `json:"project_id"`
	JobID         string `json:"job_id"`
	BaseRef       string `json:"base_ref"`
	RepositoryURL string `json:"repository_url"`
	Backend       string `json:"backend"`
	RequestHash   string `json:"request_hash"`
}

func expectedWorkspace(job Job) (workspaceManifest, error) {
	if !workspaceID.MatchString(job.TenantID) || job.TenantID[:2] != "t_" ||
		!workspaceID.MatchString(job.ProjectID) || job.ProjectID[:2] != "p_" ||
		!workspaceID.MatchString(job.ID) || job.ID[:2] != "j_" ||
		!refPattern.MatchString(job.BaseRef) || job.Backend != "api" ||
		validateProject(Project{Name: "workspace", RepositoryURL: job.RepositoryURL, BaseRef: job.BaseRef, Backend: job.Backend}) != nil {
		return workspaceManifest{}, errors.New("invalid claimed job identity")
	}
	hash, err := requestHash(Submission{Prompt: job.Prompt, Options: job.Options, Backend: job.Backend, BaseRef: job.BaseRef})
	if err != nil {
		return workspaceManifest{}, err
	}
	return workspaceManifest{Version: 2, TenantID: job.TenantID, ProjectID: job.ProjectID,
		JobID: job.ID, BaseRef: job.BaseRef, RepositoryURL: job.RepositoryURL,
		Backend: job.Backend, RequestHash: hex.EncodeToString(hash)}, nil
}

func openWorkspaceRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("workspace root must be an absolute existing directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace root must be a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	return root, nil
}

func ensureWorkspaceDirectory(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", name)
	}
	return nil
}

// prepareWorkspace binds a claimed job to a retained directory before model
// execution. A missing or altered manifest after a checkpoint is never rebuilt.
func prepareWorkspace(rootPath string, job Job, hasCheckpoint bool) (string, error) {
	expected, err := expectedWorkspace(job)
	if err != nil {
		return "", err
	}
	root, err := openWorkspaceRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, name := range []string{".hrn-control", "workspaces"} {
		if err := ensureWorkspaceDirectory(root, name); err != nil {
			return "", fmt.Errorf("workspace layout: %w", err)
		}
	}
	workspace := filepath.Join("workspaces", job.ID)
	manifest := filepath.Join(".hrn-control", job.ID+".json")
	info, err := root.Lstat(manifest)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if hasCheckpoint {
			return "", errors.New("checkpoint exists but workspace manifest is missing")
		}
		if _, err := root.Lstat(workspace); err == nil {
			return "", errors.New("workspace exists without a manifest")
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := root.Mkdir(workspace, 0o700); err != nil {
			return "", err
		}
		// O_EXCL ensures two coordinators cannot both establish this identity.
		file, err := root.OpenFile(manifest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		payload, err := json.Marshal(expected)
		if err == nil {
			_, err = file.Write(append(payload, '\n'))
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	case err != nil:
		return "", err
	default:
		if !info.Mode().IsRegular() {
			return "", errors.New("workspace manifest is not a regular file")
		}
		file, err := root.Open(manifest)
		if err != nil {
			return "", err
		}
		payload, readErr := io.ReadAll(io.LimitReader(file, 4097))
		closeErr := file.Close()
		if readErr != nil {
			return "", readErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if len(payload) > 4096 {
			return "", errors.New("workspace manifest is oversized")
		}
		var saved workspaceManifest
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&saved); err != nil {
			return "", fmt.Errorf("invalid workspace manifest: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return "", errors.New("workspace manifest has extra content")
		}
		if saved != expected {
			return "", errors.New("workspace identity differs from claimed job")
		}
	}
	info, err = root.Lstat(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace directory is not a real directory")
	}
	return filepath.Join(rootPath, workspace), nil
}
