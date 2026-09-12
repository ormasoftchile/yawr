package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// rpcRequest is an outbound JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is an inbound JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcCodec sends requests and reads responses over stdio.
type rpcCodec struct {
	mu       sync.Mutex
	enc      *json.Encoder
	dec      *json.Decoder
	nextID   int
	pending  map[int]chan rpcResponse
	readErr  error
	readDone chan struct{}
}

func newRPCCodec(r io.Reader, w io.Writer) *rpcCodec {
	c := &rpcCodec{
		enc:      json.NewEncoder(w),
		dec:      json.NewDecoder(r),
		nextID:   1,
		pending:  make(map[int]chan rpcResponse),
		readDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *rpcCodec) call(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		return fmt.Errorf("rpc call: nil context")
	}
	var payload json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		payload = data
	}

	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return err
	}
	id := c.nextID
	c.nextID++
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: payload}
	err := c.enc.Encode(req)
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return c.readError()
		}
		if resp.Error != nil {
			return fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.readDone:
		return c.readError()
	}
}

func (c *rpcCodec) readError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return io.EOF
}

func (c *rpcCodec) readLoop() {
	defer close(c.readDone)
	for {
		var resp rpcResponse
		if err := c.dec.Decode(&resp); err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
			close(ch)
		}
	}
}
