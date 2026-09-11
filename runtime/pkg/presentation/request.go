package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func closedRequest(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := uniqueRequestValue(d, 0)
	if err != nil {
		return false
	}
	if _, err = d.Token(); err != io.EOF {
		return false
	}
	object, ok := v.(map[string]any)
	if !ok || len(object) != 5 {
		return false
	}
	for _, key := range []string{"schema_version", "request_id", "context", "document", "overlays"} {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	context, ok := object["context"].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"project_root", "generation"} {
		if _, ok := context[key]; !ok {
			return false
		}
	}
	if _, ok := context["generation"].(json.Number); !ok {
		return false
	}
	for _, key := range []string{"project_root", "package_map_path", "entrypoint_path", "package_root"} {
		if value, present := context[key]; present {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	validBuffer := func(v any) bool {
		b, ok := v.(map[string]any)
		if !ok || len(b) != 4 {
			return false
		}
		for _, key := range []string{"uri", "path", "version", "text"} {
			if _, ok := b[key]; !ok {
				return false
			}
		}
		if _, ok := b["version"].(json.Number); !ok {
			return false
		}
		for _, key := range []string{"uri", "path", "text"} {
			if _, ok := b[key].(string); !ok {
				return false
			}
		}
		return true
	}
	if !validBuffer(object["document"]) {
		return false
	}
	overlays, ok := object["overlays"].([]any)
	if !ok {
		return false
	}
	for _, b := range overlays {
		if !validBuffer(b) {
			return false
		}
	}
	return true
}
func uniqueRequestValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("invalid-request")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := key.(string)
			if !ok {
				return nil, errors.New("invalid-request")
			}
			if _, ok := m[s]; ok {
				return nil, errors.New("invalid-request")
			}
			v, err := uniqueRequestValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[s] = v
		}
		_, err = d.Token()
		return m, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := uniqueRequestValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	default:
		return t, nil
	}
}
