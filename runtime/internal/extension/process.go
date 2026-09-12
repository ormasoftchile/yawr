package extension

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

const shutdownTimeout = 10 * time.Second

// extensionProcess manages a single extension subprocess.
type extensionProcess struct {
	decl   extension.ExtensionDecl
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	codec  *rpcCodec
	state  extension.ExtensionState
	mu     sync.Mutex
}

func startProcess(ctx context.Context, decl extension.ExtensionDecl) (*extensionProcess, error) {
	if decl.Entrypoint == "" {
		return nil, fmt.Errorf("extension %s: missing entrypoint", decl.Name)
	}
	entrypoint := decl.Entrypoint
	if !filepath.IsAbs(entrypoint) && strings.Contains(entrypoint, string(os.PathSeparator)) {
		entrypoint = filepath.Join(decl.Path, entrypoint)
	}
	if !filepath.IsAbs(entrypoint) && !strings.Contains(entrypoint, string(os.PathSeparator)) {
		candidate := filepath.Join(decl.Path, entrypoint)
		if _, err := os.Stat(candidate); err == nil {
			entrypoint = candidate
		}
	}
	cmd := exec.CommandContext(ctx, entrypoint)
	cmd.Dir = decl.Path
	cmd.Env = os.Environ()
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	proc := &extensionProcess{
		decl:   decl,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		state:  extension.StateStarting,
	}
	proc.codec = newRPCCodec(stdout, stdin)
	return proc, nil
}

func (p *extensionProcess) stop(ctx context.Context) error {
	p.mu.Lock()
	cmd := p.cmd
	codec := p.codec
	p.mu.Unlock()
	if cmd == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	if codec != nil {
		var result any
		_ = codec.call(shutdownCtx, "extension/shutdown", nil, &result)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	select {
	case err := <-waitCh:
		p.setState(extension.StateShutdown)
		return err
	case <-shutdownCtx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		err := <-waitCh
		p.setState(extension.StateShutdown)
		if shutdownCtx.Err() != nil {
			return shutdownCtx.Err()
		}
		return err
	}
}

func (p *extensionProcess) setState(s extension.ExtensionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = s
}

func (p *extensionProcess) getState() extension.ExtensionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}
