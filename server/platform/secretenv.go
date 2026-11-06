package platform

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// SecretEnv permits file-mounted secrets without putting their contents in the environment.
func SecretEnv(name string) (string, error) {
	value := os.Getenv(name)
	fileName := name + "_FILE"
	path := os.Getenv(fileName)
	if value != "" && path != "" {
		return "", fmt.Errorf("set %s or %s, not both", name, fileName)
	}
	if path == "" {
		return value, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", fileName, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %q: must be a regular file", fileName, path)
	}
	const maxSize = 64 * 1024
	if info.Size() > maxSize {
		return "", fmt.Errorf("%s %q: exceeds 64 KiB", fileName, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", fileName, path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", fileName, path, err)
	}
	if len(data) > maxSize {
		return "", fmt.Errorf("%s %q: exceeds 64 KiB", fileName, path)
	}
	value = string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		return "", fmt.Errorf("%s %q: secret is empty", fileName, path)
	}
	return value, nil
}
