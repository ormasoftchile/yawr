package parser

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{name: "json path", src: "json.items[0].name"},
		{name: "nested output path", src: "outputs.result.status"},
		{name: "header", src: "http.headers.content-type"},
		{name: "bad exit suffix", src: "exit_code.status", wantErr: "GCP-PARSE-002"},
		{name: "negative index", src: "json.items[-1]", wantErr: "GCP-PARSE-003"},
		{name: "trailing dot", src: "json.foo.", wantErr: "GCP-PARSE-004"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.src)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse() error = nil, want %s", tt.wantErr)
				}
				if got := err.(interface{ Code() string }).Code(); got != tt.wantErr {
					t.Fatalf("Parse() code = %s, want %s", got, tt.wantErr)
				}
			}
		})
	}
}
