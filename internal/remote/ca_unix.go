//go:build unix

package remote

import "syscall"

const caOpenFlags = syscall.O_NONBLOCK
