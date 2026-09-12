// Package resultsdelivery authorizes complete canonical publications, never previews.
package resultsdelivery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const ChunkBytes = 64 << 10

type Unavailable struct {
	status unavailableStatus
	reason unavailableReason
}

type unavailableStatus string
type unavailableReason string

const (
	statusUnavailable unavailableStatus = "unavailable"
	statusRedacted    unavailableStatus = "redacted"

	reasonNoPublication         unavailableReason = "no-publication"
	reasonInvalidPublication    unavailableReason = "invalid-publication"
	reasonProtectedContent      unavailableReason = "protected-content"
	reasonExecutionNotCompleted unavailableReason = "execution-not-completed"
	reasonProtectionUnavailable unavailableReason = "protection-unavailable"
)

type Reference struct {
	SchemaVersion string `json:"schema_version"`
	PublicationID string `json:"publication_id"`
	Digest        string `json:"digest"`
	TotalBytes    int    `json:"total_bytes"`
}

func NoPublication() *Unavailable {
	return unavailable(statusUnavailable, reasonNoPublication)
}

func InvalidPublication() *Unavailable {
	return unavailable(statusUnavailable, reasonInvalidPublication)
}

func ProtectedContent() *Unavailable {
	return unavailable(statusRedacted, reasonProtectedContent)
}

func ExecutionNotCompleted() *Unavailable {
	return unavailable(statusUnavailable, reasonExecutionNotCompleted)
}

func ProtectionUnavailable() *Unavailable {
	return unavailable(statusUnavailable, reasonProtectionUnavailable)
}

func unavailable(status unavailableStatus, reason unavailableReason) *Unavailable {
	return &Unavailable{status: status, reason: reason}
}

func (u Unavailable) Status() string {
	return string(u.status)
}

func (u Unavailable) Reason() string {
	return string(u.reason)
}

func (u Unavailable) validate() error {
	switch u.reason {
	case reasonNoPublication, reasonInvalidPublication, reasonExecutionNotCompleted, reasonProtectionUnavailable:
		if u.status == statusUnavailable {
			return nil
		}
	case reasonProtectedContent:
		if u.status == statusRedacted {
			return nil
		}
	}
	return fmt.Errorf("invalid results unavailable status/reason pair")
}

func (u Unavailable) MarshalJSON() ([]byte, error) {
	if err := u.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Status unavailableStatus `json:"status"`
		Reason unavailableReason `json:"reason"`
	}{Status: u.status, Reason: u.reason})
}

func (u *Unavailable) UnmarshalJSON(data []byte) error {
	if u == nil {
		return fmt.Errorf("nil results unavailable receiver")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("results unavailable must be an object")
	}
	var status *unavailableStatus
	var reason *unavailableReason
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("invalid results unavailable member")
		}
		switch key {
		case "status":
			if status != nil {
				return fmt.Errorf("duplicate results unavailable status")
			}
			var value unavailableStatus
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			status = &value
		case "reason":
			if reason != nil {
				return fmt.Errorf("duplicate results unavailable reason")
			}
			var value unavailableReason
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			reason = &value
		default:
			return fmt.Errorf("unknown results unavailable member %q", key)
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid results unavailable object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	if status == nil || reason == nil {
		return fmt.Errorf("results unavailable requires status and reason")
	}
	next := Unavailable{status: *status, reason: *reason}
	if err := next.validate(); err != nil {
		return err
	}
	*u = next
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func Prepare(record *engine.RunResults, cloneErr error, status engine.RunStatus, protection engine.DebugProtection) (json.RawMessage, *Unavailable) {
	if cloneErr != nil {
		return nil, InvalidPublication()
	}
	if record == nil {
		return nil, NoPublication()
	}
	for _, output := range record.Outputs {
		if output.Type == "secret" {
			return nil, ProtectedContent()
		}
		if err := executor.ValidateStrictValue(output.Value, output.Type, nil); err != nil {
			return nil, InvalidPublication()
		}
	}
	if status != engine.RunStatusCompleted {
		return nil, ExecutionNotCompleted()
	}
	if err := record.Validate(); err != nil {
		return nil, InvalidPublication()
	}
	body, err := engine.CanonicalResultsJSON(record, true)
	if err != nil {
		return nil, InvalidPublication()
	}
	if err := debugprotect.ValidateHandoffJSON(protection, body); err != nil {
		return nil, ProtectedContent()
	}
	return body, nil
}
