//go:build windows

package testutil

import (
	"errors"
	"syscall"
)

func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY)
}
