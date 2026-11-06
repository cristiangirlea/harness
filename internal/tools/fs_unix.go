//go:build unix

package tools

import "syscall"

const readOpenFlags = syscall.O_NONBLOCK
