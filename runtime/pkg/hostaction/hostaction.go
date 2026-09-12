// Package hostaction defines the typed contract between runbooks and a
// capable host surface. It deliberately carries no command identifiers or
// host-specific payloads.
package hostaction

import (
	"context"
	"encoding/json"
	"fmt"
)

type stepIDContextKey struct{}

const (
	MaxPayloadDepth            = 8
	MaxPayloadNodes            = 512
	MaxPayloadStringBytes      = 4096
	MaxPayloadKeyBytes         = 256
	MaxPayloadArrayItems       = 64
	MaxPayloadObjectProperties = 64
	MaxPayloadJSONBytes        = 64 * 1024
)

// Capability is an allowlisted host feature that a runbook may request.
type Capability string

// Status is the complete, non-sensitive result vocabulary for a host action.
type Status string

const (
	// Generic yawr.host-action/v1 bridge acknowledgement statuses.
	// These are the five status values in the yawr.host-action/v1 contract.
	// (§3.4) and are the only values a conforming bridge may emit in an ack.
	StatusCompleted           Status = "completed"
	StatusFailed              Status = "failed"
	StatusTimedOut            Status = "timed-out"
	StatusExecutionNotStarted Status = "execution-not-started"
	// StatusUnsupported is returned by the bridge when the requested capability
	// is not in the static capability registry. It is also used as a
	// framework-local headless outcome (no provider). The extension MAY also
	// return this status in an ack when it has no registered handler.
	StatusUnsupported Status = "unsupported"
)

// Request is the generic host-action request emitted by an engine.
type Request struct {
	Capability Capability     `json:"capability"`
	Payload    map[string]any `json:"request"`
}

// Response is the generic acknowledgement returned by a capable host.
type Response struct {
	Status Status         `json:"status"`
	Result map[string]any `json:"result,omitempty"`
}

// Provider executes a typed host action. A nil provider means this runtime has
// no host integration and must not attempt host work.
type Provider interface {
	ExecuteHostAction(context.Context, Request) (Response, error)
}

// ValidatePayload rejects non-JSON or oversized capability data.
func ValidatePayload(payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("host-action payload is not JSON-compatible: %w", err)
	}
	if len(encoded) > MaxPayloadJSONBytes {
		return fmt.Errorf("host-action payload exceeds %d bytes", MaxPayloadJSONBytes)
	}
	nodes := 0
	return validateValue(payload, 1, &nodes)
}

func validateValue(value any, depth int, nodes *int) error {
	if depth > MaxPayloadDepth {
		return fmt.Errorf("host-action payload exceeds depth %d", MaxPayloadDepth)
	}
	*nodes++
	if *nodes > MaxPayloadNodes {
		return fmt.Errorf("host-action payload exceeds %d values", MaxPayloadNodes)
	}

	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return nil
	case string:
		if len([]byte(typed)) > MaxPayloadStringBytes {
			return fmt.Errorf("host-action string exceeds %d bytes", MaxPayloadStringBytes)
		}
		return nil
	case []any:
		if len(typed) > MaxPayloadArrayItems {
			return fmt.Errorf("host-action array exceeds %d items", MaxPayloadArrayItems)
		}
		for _, item := range typed {
			if err := validateValue(item, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if len(typed) > MaxPayloadObjectProperties {
			return fmt.Errorf("host-action object exceeds %d properties", MaxPayloadObjectProperties)
		}
		for key, item := range typed {
			if len([]byte(key)) > MaxPayloadKeyBytes {
				return fmt.Errorf("host-action key exceeds %d bytes", MaxPayloadKeyBytes)
			}
			if err := validateValue(item, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("host-action payload contains unsupported value %T", value)
	}
}

// WithStepID associates a host action with its current runbook step without
// adding execution metadata to the host request payload.
func WithStepID(ctx context.Context, stepID string) context.Context {
	return context.WithValue(ctx, stepIDContextKey{}, stepID)
}

// StepIDFromContext returns the runbook step associated by WithStepID.
func StepIDFromContext(ctx context.Context) string {
	stepID, _ := ctx.Value(stepIDContextKey{}).(string)
	return stepID
}

// IsKnownStatus reports whether status is part of the host-action bridge contract.
func IsKnownStatus(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusTimedOut, StatusExecutionNotStarted, StatusUnsupported:
		return true
	default:
		return false
	}
}
