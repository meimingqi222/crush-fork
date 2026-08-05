//go:build !windows

package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

type unixProcess struct {
	file *os.File
	cmd  *exec.Cmd
}

func (nativeFactory) Start(_ context.Context, request OpenRequest) (Process, error) {
	cmd := exec.Command(request.Command, request.Args...)
	cmd.Dir = request.CWD
	cmd.Env = mergedEnvironment(request.Env)
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(request.Cols), Rows: uint16(request.Rows)})
	if err != nil {
		return nil, err
	}
	return &unixProcess{file: file, cmd: cmd}, nil
}

func (p *unixProcess) Read(value []byte) (int, error)  { return p.file.Read(value) }
func (p *unixProcess) Write(value []byte) (int, error) { return p.file.Write(value) }
func (p *unixProcess) Resize(cols, rows int) error {
	return pty.Setsize(p.file, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixProcess) Kill(signal string) error {
	if p.cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGKILL
	if signal == "interrupt" {
		sig = syscall.SIGINT
	} else if signal == "terminate" {
		sig = syscall.SIGTERM
	}
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *unixProcess) Wait(context.Context) (ProcessExit, error) {
	err := p.cmd.Wait()
	result := ProcessExit{Code: p.cmd.ProcessState.ExitCode()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = status.Signal().String()
		}
	}
	return result, err
}
func (p *unixProcess) Close() error { return p.file.Close() }
