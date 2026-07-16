//go:build windows

package terminal

import (
	"context"
	"sync"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type windowsProcess struct {
	pty        *conpty.ConPty
	mu         sync.Mutex
	killSignal string
	closeOnce  sync.Once
	closeErr   error
}

func (nativeFactory) Start(_ context.Context, request OpenRequest) (Process, error) {
	commandLine := windows.ComposeCommandLine(append([]string{request.Command}, request.Args...))
	value, err := conpty.Start(commandLine,
		conpty.ConPtyDimensions(request.Cols, request.Rows),
		conpty.ConPtyWorkDir(request.CWD),
		conpty.ConPtyEnv(mergedEnvironment(request.Env)),
	)
	if err != nil {
		return nil, err
	}
	return &windowsProcess{pty: value}, nil
}

func (p *windowsProcess) Read(value []byte) (int, error)  { return p.pty.Read(value) }
func (p *windowsProcess) Write(value []byte) (int, error) { return p.pty.Write(value) }
func (p *windowsProcess) Resize(cols, rows int) error     { return p.pty.Resize(cols, rows) }
func (p *windowsProcess) Kill(signal string) error {
	p.mu.Lock()
	p.killSignal = signal
	p.mu.Unlock()
	return p.Close()
}
func (p *windowsProcess) Wait(ctx context.Context) (ProcessExit, error) {
	code, err := p.pty.Wait(ctx)
	p.mu.Lock()
	signal := p.killSignal
	p.mu.Unlock()
	return ProcessExit{Code: int(code), Signal: signal}, err
}
func (p *windowsProcess) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.pty.Close() })
	return p.closeErr
}
