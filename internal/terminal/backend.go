package terminal

import (
	"context"
	"io"
)

type ProcessExit struct {
	Code   int
	Signal string
}

type Process interface {
	io.Reader
	io.Writer
	Resize(cols, rows int) error
	Kill(signal string) error
	Wait(context.Context) (ProcessExit, error)
	Close() error
}

type Factory interface {
	Start(context.Context, OpenRequest) (Process, error)
}

type nativeFactory struct{}
