//go:build !windows

package batch

import (
	"errors"
	"os"
)

func renameBusy(err error) bool { return errors.Is(err, os.ErrPermission) }
