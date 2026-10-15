package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const maxCAFileSize = 1 << 20

func readCAFile(path string) ([]byte, error) {
	// Nonblocking open lets Stat reject a FIFO even if it replaced a regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|caOpenFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open HRN_SERVER_CA_FILE: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat HRN_SERVER_CA_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCAFileSize {
		return nil, errors.New("HRN_SERVER_CA_FILE must name a regular PEM CA file of at most 1 MiB")
	}
	return readCAPEM(f)
}

func readCAPEM(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCAFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read HRN_SERVER_CA_FILE: %w", err)
	}
	if len(data) > maxCAFileSize {
		return nil, errors.New("HRN_SERVER_CA_FILE must name a regular PEM CA file of at most 1 MiB")
	}
	return data, nil
}
