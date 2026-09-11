package sensitive

import "testing"

func TestName(t *testing.T) {
	for _, value := range []string{"private_key", "access-key", "clientKey", "authHeader", "apiToken"} {
		if !Name(value) {
			t.Errorf("Name(%q) = false", value)
		}
	}
	for _, value := range []string{"monkey", "keynote", "author", "tokenize"} {
		if Name(value) {
			t.Errorf("Name(%q) = true", value)
		}
	}
}
