package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"unicode/utf8"
)

const RunResultsSchemaV1 = "yawr.run-results/v1"
const MaxResultsBytes = 256 << 20

type NamedResultValue struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type ResultsOrigin struct {
	NodeID     string `json:"node_id"`
	FrameID    string `json:"frame_id,omitempty"`
	Invocation int    `json:"invocation"`
}

type RunResults struct {
	SchemaVersion      string                      `json:"schema_version"`
	PublicationID      string                      `json:"publication_id"`
	PlanSnapshotDigest string                      `json:"plan_snapshot_digest"`
	CheckpointSequence int64                       `json:"checkpoint_sequence"`
	Origin             ResultsOrigin               `json:"origin"`
	Outputs            map[string]NamedResultValue `json:"outputs"`
	Digest             string                      `json:"digest,omitempty"`
}

// CanonicalResultsJSON sorts every object key, including record metadata.
// Array order and present null values are retained.
func CanonicalResultsJSON(results *RunResults, includeDigest bool) ([]byte, error) {
	if results == nil {
		return []byte("null"), nil
	}
	origin := map[string]any{"node_id": results.Origin.NodeID, "invocation": results.Origin.Invocation}
	if results.Origin.FrameID != "" {
		origin["frame_id"] = results.Origin.FrameID
	}
	var outputs map[string]any
	if results.Outputs != nil {
		outputs = make(map[string]any, len(results.Outputs))
		for name, value := range results.Outputs {
			outputs[name] = map[string]any{"type": value.Type, "value": value.Value}
		}
	}
	value := map[string]any{"schema_version": results.SchemaVersion, "publication_id": results.PublicationID,
		"plan_snapshot_digest": results.PlanSnapshotDigest, "checkpoint_sequence": results.CheckpointSequence,
		"origin": origin, "outputs": outputs}
	if includeDigest && results.Digest != "" {
		value["digest"] = results.Digest
	}
	var body bytes.Buffer
	if err := appendResultsJSON(&body, reflect.ValueOf(value), 0); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// Bound construction before allocating escaped strings or a complete JSON frame.
func appendResultsJSON(out *bytes.Buffer, value reflect.Value, depth int) error {
	if depth > 256 {
		return fmt.Errorf("results JSON nesting exceeds 256")
	}
	write := func(data []byte) error {
		if len(data) > MaxResultsBytes-out.Len() {
			return fmt.Errorf("results JSON exceeds %d bytes", MaxResultsBytes)
		}
		out.Write(data)
		return nil
	}
	if !value.IsValid() || ((value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer || value.Kind() == reflect.Map || value.Kind() == reflect.Slice) && value.IsNil()) {
		return write([]byte("null"))
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		return appendResultsJSON(out, value.Elem(), depth+1)
	}
	if number, ok := value.Interface().(json.Number); ok {
		if len(number) > MaxResultsBytes-out.Len() {
			return fmt.Errorf("results JSON exceeds %d bytes", MaxResultsBytes)
		}
		data, err := json.Marshal(number)
		if err != nil {
			return err
		}
		return write(data)
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("results JSON requires string object keys")
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		if err := write([]byte("{")); err != nil {
			return err
		}
		for i, key := range keys {
			if i > 0 {
				if err := write([]byte(",")); err != nil {
					return err
				}
			}
			if err := appendResultsJSON(out, reflect.ValueOf(key.String()), depth+1); err != nil {
				return err
			}
			if err := write([]byte(":")); err != nil {
				return err
			}
			if err := appendResultsJSON(out, value.MapIndex(key), depth+1); err != nil {
				return err
			}
		}
		return write([]byte("}"))
	case reflect.Array, reflect.Slice:
		if err := write([]byte("[")); err != nil {
			return err
		}
		for i := 0; i < value.Len(); i++ {
			if i > 0 {
				if err := write([]byte(",")); err != nil {
					return err
				}
			}
			if err := appendResultsJSON(out, value.Index(i), depth+1); err != nil {
				return err
			}
		}
		return write([]byte("]"))
	case reflect.String:
		text := value.String()
		if !utf8.ValidString(text) {
			return fmt.Errorf("results JSON contains invalid UTF-8")
		}
		size := 2
		for len(text) > 0 {
			r, n := utf8.DecodeRuneInString(text)
			text = text[n:]
			switch {
			case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
				size += 2
			case r < 32 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029' || r == utf8.RuneError && n == 1:
				size += 6
			default:
				size += n
			}
			if size > MaxResultsBytes-out.Len() {
				return fmt.Errorf("results JSON exceeds %d bytes", MaxResultsBytes)
			}
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	default:
		return fmt.Errorf("unsupported results JSON value %s", value.Kind())
	}
	data, err := json.Marshal(value.Interface())
	if err != nil {
		return err
	}
	return write(data)
}

func (results *RunResults) Seal() error {
	data, err := CanonicalResultsJSON(results, false)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	results.Digest = fmt.Sprintf("sha256:%x", digest)
	return nil
}

func (results *RunResults) Validate() error {
	if results == nil || results.SchemaVersion != RunResultsSchemaV1 ||
		results.PublicationID == "" || results.PlanSnapshotDigest == "" ||
		results.CheckpointSequence < 1 || results.Origin.NodeID == "" ||
		results.Origin.Invocation < 1 || results.Outputs == nil || results.Digest == "" {
		return fmt.Errorf("invalid yawr.run-results/v1 identity")
	}
	copy := *results
	if err := copy.Seal(); err != nil {
		return err
	}
	if copy.Digest != results.Digest {
		return fmt.Errorf("yawr.run-results/v1 digest mismatch")
	}
	return nil
}

func CloneRunResults(results *RunResults) (*RunResults, error) {
	if results == nil {
		return nil, nil
	}
	data, err := CanonicalResultsJSON(results, true)
	if err != nil {
		return nil, err
	}
	var copy RunResults
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&copy); err != nil {
		return nil, err
	}
	return &copy, nil
}
