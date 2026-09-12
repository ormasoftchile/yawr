package extension

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

var pingInterval = int64((30 * time.Second).Nanoseconds())
var pingTimeout = int64((5 * time.Second).Nanoseconds())

// startHealthLoop begins a background goroutine that pings the extension every 30s.
// On ping failure, sets state to StateFailed and closes the process.
func startHealthLoop(ctx context.Context, proc *extensionProcess, onFailure func(name string, err error)) {
	if proc == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(loadPingInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pingOnce(ctx, proc); err != nil {
					proc.setState(extension.StateFailed)
					if onFailure != nil {
						onFailure(proc.decl.Name, err)
					}
					_ = proc.stop(context.Background())
					return
				}
			}
		}
	}()
}

func pingOnce(parent context.Context, proc *extensionProcess) error {
	ctx, cancel := context.WithTimeout(parent, loadPingTimeout())
	defer cancel()
	var resp string
	if err := proc.codec.call(ctx, "extension/ping", nil, &resp); err != nil {
		return err
	}
	if resp != "pong" {
		return fmt.Errorf("unexpected ping response: %s", resp)
	}
	return nil
}
func loadPingInterval() time.Duration {
	return time.Duration(atomic.LoadInt64(&pingInterval))
}

func loadPingTimeout() time.Duration {
	return time.Duration(atomic.LoadInt64(&pingTimeout))
}
