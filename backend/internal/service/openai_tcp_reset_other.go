//go:build !windows

package service

import (
	"errors"
	"syscall"
)

func isOpenAITCPResetError(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}
