package handlers

import (
	"io"
)

type TtyHandler interface {
	io.Closer
	Run(errChan chan<- error)
	HandleDisconnect() error
	HandleReconnect() error
}
