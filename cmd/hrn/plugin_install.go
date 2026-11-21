package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/plugin"
	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runpaths"
	"github.com/cristiangirlea/harness/internal/update"
	"github.com/cristiangirlea/harness/internal/ws"
)

var (
	pluginReleaseAPIBase = "https://api.github.com"
	pluginReleaseTag     = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]{0,63}$`)
)

const pluginInstallRecordName = "install.json"

type pluginInstallRecord struct {
	Source      string `json:"source"`
	Tag         string `json:"tag"`
	Asset       string `json:"asset"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installed_at"`
}

func installCommandPluginRelease(ctx context.Context, stdout io.Writer, arg string, override *string, jsonOut bool) (int, error) {
	home, err := runpaths.ResolveHome()
	if err != nil {
		return 1, err
	}
	layers, err := resolve.BackendLayers(home)
	if err != nil {
		return 1, err
	}
	if layers.Effective.PluginInstallOff() {
		file := "your backends.json"
		if layers.Org.Locks("plugins") || layers.User.Plugins == nil {
			file = "the organisation's backends.json"
		}
		return 1, fmt.Errorf("plugin installation is turned off by %s", file)
	}
	ref, tag, tagged := strings.Cut(arg, "@")
	if tagged && !pluginReleaseTag.MatchString(tag) {
		return 2, fmt.Errorf("invalid release tag %q", tag)
	}
	name, source, err := resolvePluginInstallSource(layers.Effective, ref, override)
	if err != nil {
		return 2, err
	}
	if !layers.Effective.PluginAllowed(source) {
		return 1, fmt.Errorf("plugin source %s is not allowed by plugins.allow", source)
	}
	info, record, err := stagePluginRelease(ctx, plugin.Root(), name, source, tag)
	if err != nil {
		return 1, err
	}
	if err := printPluginInstall(stdout, info, record, jsonOut); err != nil {
		return 1, err
	}
	return 0, nil
}

func resolvePluginInstallSource(p prefs.Prefs, ref string, override *string) (name, source string, err error) {
	if _, repo, explicit := strings.Cut(ref, "/"); explicit {
		if !prefs.ValidPluginSource(ref) {
			return "", "", fmt.Errorf("invalid plugin source %q: want OWNER/REPO", ref)
		}
		name, source = strings.TrimPrefix(repo, "hrn-"), ref
		if override != nil {
			name = *override
		}
	} else {
		if override != nil {
			return "", "", errors.New("--name requires OWNER/REPO")
		}
		name = ref
		source, _ = p.PluginSource(name)
	}
	if !prefs.ValidPluginName(name) {
		return "", "", fmt.Errorf("invalid or reserved plugin name %q", name)
	}
	if source == "" {
		return "", "", fmt.Errorf("no source is configured for plugin %s (use hrn plugin install OWNER/REPO)", name)
	}
	return name, source, nil
}

func stagePluginRelease(ctx context.Context, root, name, source, requestedTag string) (plugin.Info, pluginInstallRecord, error) {
	var info plugin.Info
	var record pluginInstallRecord
	if err := os.MkdirAll(root, 0o700); err != nil {
		return info, record, err
	}
	stage, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return info, record, err
	}
	defer os.RemoveAll(stage)

	api := pluginReleaseAPIBase + "/repos/" + source + "/releases/latest"
	if requestedTag != "" {
		api = pluginReleaseAPIBase + "/repos/" + source + "/releases/tags/" + url.PathEscape(requestedTag)
	}
	release := update.ReleaseSource{API: api, Asset: func(tag, goos, goarch string) string {
		if !pluginReleaseTag.MatchString(tag) {
			return ""
		}
		return "hrn-" + name + strings.TrimPrefix(update.AssetName(tag, goos, goarch), "hrn")
	}}
	download, tag, err := release.Fetch(ctx, stage)
	if err != nil {
		return info, record, err
	}
	if requestedTag != "" && tag != requestedTag {
		return info, record, fmt.Errorf("release returned tag %q, want %q", tag, requestedTag)
	}
	stagedExe := filepath.Join(stage, plugin.ExecutableName(name))
	if err := os.Rename(download, stagedExe); err != nil {
		return info, record, err
	}
	dir, err := os.Getwd()
	if err != nil {
		return info, record, err
	}
	session, err := plugin.Start(ctx, plugin.Found{Name: name, Path: stagedExe}, plugin.Options{
		Host: pluginproto.Program{Name: "hrn", Version: agent.Version},
		Dir:  dir,
	})
	if err != nil {
		return info, record, err
	}
	// Start has already checked that the ready frame names the requested plugin.
	info = session.Info()
	if err := session.Close(); err != nil {
		return info, record, err
	}
	if err := ctx.Err(); err != nil {
		return info, record, err
	}
	record = pluginInstallRecord{
		Source: source, Tag: tag, Asset: filepath.Base(download), SHA256: info.SHA256,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	info.Path = filepath.Join(root, name, plugin.ExecutableName(name))
	if err := publishPluginInstall(root, stage, stagedExe, info.Path, record); err != nil {
		return info, record, err
	}
	return info, record, nil
}

func publishPluginInstall(root, stage, stagedExe, targetExe string, record pluginInstallRecord) error {
	targetDir := filepath.Dir(targetExe)
	unlock, ok, err := ws.TryLock(filepath.Join(root, ".install-"+filepath.Base(targetDir)+".lock"))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("an installation for %s is already in progress", filepath.Base(targetDir))
	}
	defer unlock()

	f, err := os.CreateTemp(stage, ".install-*.json")
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(f).Encode(record)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	stagedRecord := filepath.Join(stage, pluginInstallRecordName)
	if err := os.Rename(f.Name(), stagedRecord); err != nil {
		return err
	}
	switch target, err := os.Lstat(targetDir); {
	case errors.Is(err, os.ErrNotExist):
		return os.Rename(stage, targetDir)
	case err != nil:
		return err
	case !target.IsDir():
		return fmt.Errorf("cannot install plugin in %s: want a directory", targetDir)
	}

	targetRecord := filepath.Join(targetDir, pluginInstallRecordName)
	if existing, err := os.Lstat(targetRecord); err == nil && !existing.Mode().IsRegular() {
		return fmt.Errorf("cannot replace plugin install record %s: want a regular file", targetRecord)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup, err := backUpInstalledPlugin(targetExe)
	if err != nil {
		return err
	}
	if err := os.Rename(stagedExe, targetExe); err != nil {
		if backup != "" {
			_ = os.Remove(backup)
		}
		if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(32))) {
			return fmt.Errorf("cannot replace %s: close the running plugin if it is in use and retry: %w", targetExe, err)
		}
		return err
	}
	if err := os.Rename(stagedRecord, targetRecord); err != nil {
		var rollbackErr error
		if backup != "" {
			rollbackErr = os.Rename(backup, targetExe)
		} else {
			rollbackErr = os.Remove(targetExe)
		}
		return errors.Join(fmt.Errorf("write plugin install record: %w", err), rollbackErr)
	}
	if backup != "" {
		_ = os.Remove(backup)
	}
	return nil
}

// Keep the previous executable available until both replacements succeed.
func backUpInstalledPlugin(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("cannot replace %s: want a regular file", path)
	}
	source, err := os.Open(path)
	if err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
			return "", fmt.Errorf("cannot open %s: close the running plugin and retry: %w", path, err)
		}
		return "", err
	}
	defer source.Close()
	backup, err := os.CreateTemp(filepath.Dir(path), ".previous-*")
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(backup, source)
	if err := errors.Join(copyErr, backup.Chmod(info.Mode().Perm()), backup.Close()); err != nil {
		_ = os.Remove(backup.Name())
		return "", err
	}
	return backup.Name(), nil
}

func installedPluginSource(found plugin.Found) string {
	if !found.Installed {
		return ""
	}
	path := filepath.Join(filepath.Dir(found.Path), pluginInstallRecordName)
	stat, err := os.Lstat(path)
	const maxRecord = 64 << 10
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > maxRecord {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	var record pluginInstallRecord
	if err != nil || len(data) > maxRecord || json.Unmarshal(data, &record) != nil || !prefs.ValidPluginSource(record.Source) {
		return ""
	}
	return record.Source
}

func missingCommandPlugin(name string, missing error) error {
	p, err := resolve.BackendPreferences(runpaths.Home())
	if err == nil && p.PluginInstallOff() {
		return fmt.Errorf("%w (place %s in $HRN_HOME/plugins/%s/ or on PATH)", missing, plugin.ExecutableName(name), name)
	}
	_, configured := p.PluginSource(name)
	return fmt.Errorf("%w (install it with: %s)", missing, plugin.InstallHint(name, err == nil && configured))
}

func printPluginInstall(stdout io.Writer, info plugin.Info, record pluginInstallRecord, jsonOut bool) error {
	if jsonOut {
		return encodeTerminalSafeJSON(stdout, struct {
			Name         string                   `json:"name"`
			Version      string                   `json:"version"`
			Tag          string                   `json:"tag"`
			Source       string                   `json:"source"`
			Path         string                   `json:"path"`
			SHA256       string                   `json:"sha256"`
			Capabilities []pluginproto.Capability `json:"capabilities"`
		}{
			Name: info.Name, Version: info.Version, Tag: record.Tag, Source: record.Source,
			Path: info.Path, SHA256: record.SHA256, Capabilities: info.Capabilities,
		})
	}
	if _, err := fmt.Fprintf(stdout, "installed %s %s from %s\npath: %s\n",
		info.Name, record.Tag, record.Source, printable(mask.Text(info.Path))); err != nil {
		return err
	}
	return printCommandPluginCapabilities(stdout, info.Capabilities)
}
