package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxCAFileSize = 1 << 20

var errCAFileRule = errors.New("HRN_SERVER_CA_FILE must name a regular PEM CA file of at most 1 MiB")

var openCAFile = os.OpenFile

func readCAFile(path string) ([]byte, error) {
	// Opening a device or pipe acts on it (a tape rewind, a pipe instance), so refuse it first.
	if windowsDevicePath(path) {
		return nil, errCAFileRule
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat HRN_SERVER_CA_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCAFileSize {
		return nil, errCAFileRule
	}
	// Nonblocking open lets Stat reject a FIFO even if it replaced a regular file.
	f, err := openCAFile(path, os.O_RDONLY|caOpenFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open HRN_SERVER_CA_FILE: %w", err)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat HRN_SERVER_CA_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCAFileSize {
		return nil, errCAFileRule
	}
	return readCAPEM(f)
}

// windowsDevicePath reports device namespace prefixes and reserved names, which Stat may open.
func windowsDevicePath(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) {
		return true
	}
	// A UNC pipe or mailslot share on this host reaches the local server through the redirector.
	if rest, ok := strings.CutPrefix(path, `\\`); ok {
		if parts := strings.SplitN(rest, `\`, 3); len(parts) >= 2 &&
			(strings.EqualFold(parts[1], "pipe") || strings.EqualFold(parts[1], "mailslot")) {
			return true
		}
	}
	name, _, _ := strings.Cut(filepath.Base(path), ".")
	// Windows reads COM1: as the device, so a trailing colon is trimmed like a space.
	name = strings.ToUpper(strings.TrimRight(name, " :"))
	switch name {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) &&
		name[3] >= '0' && name[3] <= '9'
}

func readCAPEM(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCAFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read HRN_SERVER_CA_FILE: %w", err)
	}
	if len(data) > maxCAFileSize {
		return nil, errCAFileRule
	}
	return data, nil
}
