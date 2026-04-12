package memory

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// RuntimeProcess manages the universal-memory runtime subprocess.
type RuntimeProcess struct {
	command string
	args    []string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader

	mu         sync.Mutex
	running    bool
	stopped    chan struct{}
	closeOnce  sync.Once // Ensure stopped is closed only once
}

// NewRuntimeProcess creates a new runtime process manager.
func NewRuntimeProcess(command string, args []string) *RuntimeProcess {
	return &RuntimeProcess{
		command: command,
		args:    args,
		stopped: make(chan struct{}),
	}
}

// Start launches the runtime process.
func (p *RuntimeProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return nil
	}

	p.cmd = exec.Command(p.command, p.args...)

	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	p.stdin = stdin

	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	p.stdout = stdout

	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	p.stderr = stderr

	// Start the process
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	p.running = true

	// Monitor stderr for logging
	go p.monitorStderr(ctx)
	go p.monitorExit(ctx)

	return nil
}

// Stop terminates the runtime process.
func (p *RuntimeProcess) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running {
		return nil
	}

	// Try graceful shutdown first via JSON-RPC
	if p.stdin != nil {
		p.stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"shutdown","params":{}}` + "\n"))
	}

	// Wait a bit for graceful shutdown
	time.Sleep(500 * time.Millisecond)

	// Force kill if still running
	if p.cmd.Process != nil {
		// On Windows, SIGTERM is not supported, so we use Kill directly
		// On Unix, we try SIGTERM first, then Kill
		p.cmd.Process.Kill()
	}

	p.running = false
	p.closeOnce.Do(func() {
		close(p.stopped)
	})

	return nil
}

// Stdin returns the stdin pipe for writing requests.
func (p *RuntimeProcess) Stdin() io.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdin
}

// Stdout returns the stdout pipe for reading responses.
func (p *RuntimeProcess) Stdout() io.Reader {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout
}

// Stderr returns the stderr pipe for logging.
func (p *RuntimeProcess) Stderr() io.Reader {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr
}

// Running returns true if the process is running.
func (p *RuntimeProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Stopped returns a channel that closes when the process stops.
func (p *RuntimeProcess) Stopped() <-chan struct{} {
	return p.stopped
}

// monitorStderr logs stderr output.
func (p *RuntimeProcess) monitorStderr(ctx context.Context) {
	scanner := bufio.NewScanner(p.stderr)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		case <-p.stopped:
			return
		default:
		}
		fmt.Printf("[memory-runtime] %s\n", scanner.Text())
	}
}

// monitorExit waits for the process to exit.
func (p *RuntimeProcess) monitorExit(ctx context.Context) {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.running = false
	p.closeOnce.Do(func() {
		close(p.stopped)
	})
	p.mu.Unlock()

	if err != nil {
		fmt.Printf("[memory-runtime] Process exited with error: %v\n", err)
	} else {
		fmt.Printf("[memory-runtime] Process exited normally\n")
	}
}
