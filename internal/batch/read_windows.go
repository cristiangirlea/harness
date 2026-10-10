package batch

import (
	"io"
	"os"
	"syscall"
)

// readRecord reads a record file without standing in the way of save. Go
// opens files without FILE_SHARE_DELETE, and while such a handle is open
// Windows refuses save's rename over the file; hrn watch reads records all
// the time, for every page and stream open on it. Opened with
// FILE_SHARE_DELETE, a reader keeps the copy it opened and the rename goes
// through.
func readRecord(path string) ([]byte, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		// A path CreateFile cannot take as it is (one past MAX_PATH, which
		// os handles for us) is still readable the ordinary way.
		if data, rerr := os.ReadFile(path); rerr == nil {
			return data, nil
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}
