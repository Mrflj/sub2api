package service

import (
	"errors"
	"syscall"
)

func isOpenAITCPResetError(err error) bool {
	// Windows sockets return the Winsock code, while wrapped portable errors
	// may still carry Go's compatibility ECONNRESET value.
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.ECONNRESET)
}
