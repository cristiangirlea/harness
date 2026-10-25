package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LatestReleaseAPI locates hrn's newest release.
const LatestReleaseAPI = "https://api.github.com/repos/cristiangirlea/harness/releases/latest"

// Cap untrusted downloads at 256 MiB.
const maxAsset = 256 << 20

// AssetName matches release.sh's hrn_<version>_<os>_<arch>[.exe], without the tag's v.
func AssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("hrn_%s_%s_%s", strings.TrimPrefix(version, "v"), goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// VerifyChecksum matches data to name's text or binary SHA256SUMS entry.
func VerifyChecksum(sums []byte, name string, data []byte) error {
	var want string
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("SHA256SUMS does not list %s", name)
	}
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("SHA256SUMS has no valid digest for %s", name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s: SHA256SUMS says %s, the download is %s", name, want, got)
	}
	return nil
}

// ReleaseSource fetches platform binaries using GitHub or a configured API/client.
// Token authenticates only the API request, never asset downloads.
type ReleaseSource struct {
	API    string
	Client *http.Client
	Token  string
	GOOS   string
	GOARCH string
}

type releaseDoc struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Fetch returns a checksum-verified binary path and release tag.
// Trust the API's SHA256SUMS; never write mismatched bytes as an executable.
func (s ReleaseSource) Fetch(ctx context.Context, dir string) (path, tag string, err error) {
	api, client, goos, goarch := s.API, s.Client, s.GOOS, s.GOARCH
	if api == "" {
		api = LatestReleaseAPI
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	body, err := s.get(ctx, client, api, "application/vnd.github+json", true)
	if err != nil {
		return "", "", fmt.Errorf("latest release: %w", err)
	}
	var doc releaseDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("latest release: %w", err)
	}
	if doc.TagName == "" {
		return "", "", errors.New("latest release: no tag_name in the response")
	}
	// Untrusted tags must not redirect writes outside dir.
	if strings.ContainsAny(doc.TagName, `/\`) || strings.Contains(doc.TagName, "..") {
		return "", "", fmt.Errorf("latest release: tag %q is not a plain version tag", doc.TagName)
	}
	name := AssetName(doc.TagName, goos, goarch)
	if filepath.Base(name) != name {
		return "", "", fmt.Errorf("latest release: tag %q is not a plain version tag", doc.TagName)
	}
	var binURL, sumsURL string
	for _, a := range doc.Assets {
		switch a.Name {
		case name:
			binURL = a.URL
		case "SHA256SUMS":
			sumsURL = a.URL
		}
	}
	if binURL == "" {
		return "", "", fmt.Errorf("release %s has no asset %s", doc.TagName, name)
	}
	if sumsURL == "" {
		return "", "", fmt.Errorf("release %s has no SHA256SUMS", doc.TagName)
	}
	for _, u := range []string{sumsURL, binURL} {
		if err := assetURLAllowed(api, u); err != nil {
			return "", "", err
		}
	}
	sums, err := s.get(ctx, client, sumsURL, "application/octet-stream", false)
	if err != nil {
		return "", "", fmt.Errorf("SHA256SUMS: %w", err)
	}
	data, err := s.get(ctx, client, binURL, "application/octet-stream", false)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", name, err)
	}
	if err := VerifyChecksum(sums, name, data); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		return "", "", err
	}
	return path, doc.TagName, nil
}

// Restrict response-supplied assets to the API origin or GitHub HTTPS hosts.
func assetURLAllowed(api, asset string) error {
	a, err := url.Parse(api)
	if err != nil {
		return err
	}
	u, err := url.Parse(asset)
	if err != nil || u.Host == "" {
		return fmt.Errorf("asset %q is not a GitHub URL", asset)
	}
	if u.Host == a.Host && u.Scheme == a.Scheme {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	github := host == "github.com" || strings.HasSuffix(host, ".github.com") ||
		host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
	if u.Scheme != "https" || !github {
		return fmt.Errorf("asset %q is not a GitHub URL over https", asset)
	}
	return nil
}

// Send tokens only to API requests; asset CDNs must never receive them.
func (s ReleaseSource) get(ctx context.Context, client *http.Client, url, accept string, withToken bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "hrn-update")
	if withToken && s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAsset {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxAsset)
	}
	return data, nil
}

func isWindows() bool { return runtime.GOOS == "windows" }
