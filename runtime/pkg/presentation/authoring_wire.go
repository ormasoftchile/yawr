package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

const AuthoringRequestVersion = "authoring-request/v3"
const AuthoringResolverVersion = "core-authoring/v3"

type AuthoringRequest struct {
	SchemaVersion string   `json:"schema_version"`
	RequestID     string   `json:"request_id"`
	Operation     string   `json:"operation"`
	Context       Context  `json:"context"`
	Document      Buffer   `json:"document"`
	Overlays      []Buffer `json:"overlays"`
	Position      int      `json:"position"`
	contextJSON   json.RawMessage
}
type AuthoringDiscovery struct {
	Scope  string `json:"scope"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
type AuthoringSite struct {
	Kind       string `json:"kind"`
	YAMLPath   string `json:"yaml_path"`
	Range      Range  `json:"range"`
	BindingID  string `json:"binding_id,omitempty"`
	ToolID     string `json:"tool_id,omitempty"`
	ToolDigest string `json:"tool_digest,omitempty"`
	Action     string `json:"action,omitempty"`
}
type AuthoringEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"new_text"`
}
type AuthoringItem struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	ValueType   string        `json:"value_type,omitempty"`
	Required    *bool         `json:"required,omitempty"`
	DefaultInfo string        `json:"default_info,omitempty"`
	Edit        AuthoringEdit `json:"edit"`
}
type AuthoringParameter struct {
	Name        string `json:"name"`
	LabelRange  Range  `json:"label_range"`
	Description string `json:"description,omitempty"`
}
type AuthoringSignature struct {
	Name            string               `json:"name"`
	Label           string               `json:"label"`
	Description     string               `json:"description,omitempty"`
	Parameters      []AuthoringParameter `json:"parameters"`
	ActiveParameter *int                 `json:"active_parameter"`
	CallRange       Range                `json:"call_range"`
}
type AuthoringRequiredEdit struct {
	Edit         AuthoringEdit `json:"edit"`
	Placeholders []Range       `json:"placeholders"`
}
type AuthoringDocument struct {
	URI     string `json:"uri"`
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}
type AuthoringReply struct {
	SchemaVersion   string                 `json:"schema_version"`
	ResolverVersion string                 `json:"resolver_version"`
	GrammarVersion  string                 `json:"grammar_version"`
	Operation       string                 `json:"operation"`
	RequestID       string                 `json:"request_id"`
	Context         Context                `json:"context"`
	Document        AuthoringDocument      `json:"document"`
	Status          string                 `json:"status"`
	Reason          string                 `json:"reason,omitempty"`
	Discovery       AuthoringDiscovery     `json:"discovery"`
	Dependencies    []Dependency           `json:"dependencies"`
	Site            *AuthoringSite         `json:"site"`
	Items           []AuthoringItem        `json:"items"`
	Signature       *AuthoringSignature    `json:"signature"`
	RequiredEdit    *AuthoringRequiredEdit `json:"required_edit"`
	contextJSON     json.RawMessage
}

func (r AuthoringReply) MarshalJSON() ([]byte, error) {
	type wire AuthoringReply
	if len(r.contextJSON) == 0 {
		return json.Marshal(wire(r))
	}
	return json.Marshal(struct {
		wire
		Context json.RawMessage `json:"context"`
	}{wire(r), r.contextJSON})
}

func AuthoringCapabilities() any {
	return AuthoringTypedCapabilities()
}

func DecodeAuthoringRequest(r io.Reader, operation string) (AuthoringRequest, error) {
	var req AuthoringRequest
	invalid := errors.New("invalid-request")
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(data) > MaxBytes || !utf8.Valid(data) || !authoringJSONUnicode(data) {
		return req, invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	value, err := uniqueRequestValue(d, 0)
	if err != nil {
		return req, invalid
	}
	if _, err = d.Token(); err != io.EOF {
		return req, invalid
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 7 {
		return req, invalid
	}
	for _, key := range []string{"schema_version", "request_id", "operation", "context", "document", "overlays", "position"} {
		if _, ok := object[key]; !ok {
			return req, invalid
		}
	}
	if _, ok := object["position"].(json.Number); !ok {
		return req, invalid
	}
	if _, ok := object["operation"].(string); !ok {
		return req, invalid
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || req.SchemaVersion != AuthoringRequestVersion || req.Operation != operation ||
		(operation != "complete" && operation != "signature" && operation != "required-arguments") {
		return req, invalid
	}
	delete(object, "operation")
	delete(object, "position")
	object["schema_version"] = SchemaVersion
	base, _ := json.Marshal(object)
	if _, err := DecodeRequest(bytes.NewReader(base)); err != nil {
		return req, invalid
	}
	req.contextJSON, _ = json.Marshal(object["context"])
	if _, ok := authoringByteOffset(req.Document.Text, req.Position); !ok {
		return req, invalid
	}
	return req, nil
}

func authoringByteOffset(s string, units int) (int, bool) {
	if units < 0 {
		return 0, false
	}
	n := 0
	for i, r := range s {
		if n == units {
			return i, true
		}
		n++
		if r > 0xffff {
			n++
		}
		if n > units {
			return 0, false
		}
	}
	return len(s), n == units
}

func authoringJSONUnicode(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		code, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
