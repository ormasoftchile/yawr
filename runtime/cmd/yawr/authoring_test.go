package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/ormasoftchile/yawr/runtime/cmd/internal/testworkspace"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func TestAuthoringIncludeCLIProtocol(t *testing.T) {
	root := authoringWorkspace(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "apiVersion: yawr.runbook/v1\nid: example\nname: Example 😀\nflow:\n  - step:\n      id: child\n      type: include\n      include:"
	for _, v := range []struct{ source, kind string }{
		{"|CURSOR| # keep\n", "include-mapping"},
		{"\n        |CURSOR|\n", "include-key"},
		{" {'ru|CURSOR|n': child.yaml}\n", "include-key"},
		{"\n        runbook_ref: child\n        resolve_from: |CURSOR|\n", "include-value"},
	} {
		source := prefix + v.source
		req := presentation.AuthoringRequest{SchemaVersion: presentation.AuthoringIncludeRequestVersion, RequestID: "include",
			Operation: "complete", Context: presentation.Context{ProjectRoot: root},
			Document: presentation.Buffer{Path: filepath.Join(root, "new.runbook.yaml"), Version: 1,
				Text: strings.Replace(source, "|CURSOR|", "", 1)}, Overlays: []presentation.Buffer{},
			Position: len(utf16.Encode([]rune(source[:strings.Index(source, "|CURSOR|")])))}
		req.Document.URI = presentation.FileURI(req.Document.Path)
		for _, version := range []string{presentation.AuthoringIncludeRequestVersion, presentation.AuthoringRequestVersion, "authoring-request/v4"} {
			req.SchemaVersion = version
			input, _ := json.Marshal(req)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPresentationDirectCLIProcess$", "--", "authoring", "complete", "--stdio")
			cmd.Env = append(os.Environ(), "YAWR_PRESENTATION_DIRECT_CHILD=1")
			cmd.Stdin = bytes.NewReader(input)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			cancel()
			if version == "authoring-request/v4" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 2 || len(out) != 0 || strings.TrimSpace(stderr.String()) != "authoring: invalid-request" {
					t.Fatal("unknown version did not fail closed")
				}
				continue
			}
			if err != nil || stderr.Len() != 0 {
				t.Fatal(err, stderr.String())
			}
			var reply presentation.AuthoringReply
			if json.Unmarshal(out, &reply) != nil || reply.Status != "resolved" {
				t.Fatal("invalid reply")
			}
			if version == presentation.AuthoringIncludeRequestVersion {
				if reply.SchemaVersion != "authoring-reply/v2" || reply.Site == nil || reply.Site.Kind != v.kind || len(reply.Items) == 0 {
					t.Fatalf("missing include reply %+v", reply)
				}
			} else if reply.SchemaVersion != "yawr.authoring-reply/v1" || reply.Site != nil || len(reply.Items) != 0 {
				t.Fatal("v1 behavior changed")
			}
		}
	}
	for _, args := range [][]string{{"authoring", "capabilities"}, {"authoring", "capabilities", "--v2"}} {
		data, stderr, err := presentationDirectCLI(t, root, args...)
		if err != nil || stderr != "" {
			t.Fatal(err, stderr)
		}
		var actual any
		if json.Unmarshal(data, &actual) != nil {
			t.Fatal("invalid capability JSON")
		}
		expected := presentation.AuthoringCapabilities()
		if len(args) == 3 {
			expected = presentation.AuthoringIncludeCapabilities()
		}
		want, _ := json.Marshal(expected)
		got, _ := json.Marshal(actual)
		var wantMap any
		json.Unmarshal(want, &wantMap)
		want, _ = json.Marshal(wantMap)
		if !bytes.Equal(got, want) {
			t.Fatal("capability contract drift")
		}
	}
}
func TestAuthoringCLIProtocol(t *testing.T) {
	repo := findRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(repo, "pkg", "presentation", "testdata", "authoring-canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	if presentation.Digest(data) != "sha256:bd37e62d80bcb59ce3fd5835f8e1b8e00dee4575649dc235fe5612c0a107554b" {
		t.Fatal("canonical drift")
	}
	var fixture struct {
		Base     presentation.AuthoringRequest `json:"base_request"`
		Document presentation.Buffer           `json:"document_identity"`
		Prefix   string
		Cases    []struct {
			Name, Operation string
			Source          string `json:"source_template"`
		}
	}
	if json.Unmarshal(data, &fixture) != nil {
		t.Fatal("fixture decode")
	}
	root := authoringWorkspace(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(operation string, input []byte) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPresentationDirectCLIProcess$", "--", "authoring", operation, "--stdio")
		cmd.Env = append(os.Environ(), "YAWR_PRESENTATION_DIRECT_CHILD=1")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil && stderr.String() != "authoring: invalid-request\n" && stderr.String() != "authoring: invalid-request\r\n" {
			t.Fatal("unexpected stderr category", stderr.String())
		}
		return out, err
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			req := fixture.Base
			req.SchemaVersion = presentation.AuthoringRequestVersion
			req.Operation = c.Operation
			req.Context.ProjectRoot = root
			req.Document = fixture.Document
			req.Document.Path = filepath.Join(root, "new.runbook.yaml")
			req.Document.URI = presentation.FileURI(req.Document.Path)
			text := fixture.Prefix + c.Source
			at := strings.Index(text, "|CURSOR|")
			req.Position = len(utf16.Encode([]rune(text[:at])))
			req.Document.Text = strings.Replace(text, "|CURSOR|", "", 1)
			req.Overlays = append([]presentation.Buffer(nil), req.Overlays...)
			req.Overlays[0].Path = filepath.Join(root, "db.tool.yaml")
			req.Overlays[0].URI = presentation.FileURI(req.Overlays[0].Path)
			input, _ := json.Marshal(req)
			out, err := invoke(c.Operation, input)
			if err != nil {
				t.Fatal(err)
			}
			var reply presentation.AuthoringReply
			if json.Unmarshal(out, &reply) != nil || reply.Status != "resolved" || reply.Operation != c.Operation || reply.Document.Digest != presentation.Digest([]byte(req.Document.Text)) {
				t.Fatal("bad production reply")
			}
			switch c.Name {
			case "tool", "action", "argument":
				if len(reply.Items) == 0 {
					t.Fatal("missing actual CLI completion")
				}
			case "signature":
				if reply.Signature == nil || reply.Signature.Name != "str.contains" {
					t.Fatal("missing signature")
				}
			case "plain-sql":
				if len(reply.Items) != 0 || reply.Site != nil {
					t.Fatal("host completion")
				}
			case "explicit-required":
				if reply.RequiredEdit == nil {
					t.Fatal("missing explicit edit")
				}
			}

			bad := append([]byte(nil), input...)
			bad = bytes.Replace(bad, []byte(`"position":`), []byte(`"position":0,"position":`), 1)
			out, err = invoke(c.Operation, bad)
			var exit *exec.ExitError
			if err == nil || len(out) != 0 {
				t.Fatal("duplicate key accepted")
			}
			exit, _ = err.(*exec.ExitError)
			if exit == nil || exit.ExitCode() != 2 {
				t.Fatal("invalid request exit code")
			}
		})
	}
	capability, stderr, err := presentationDirectCLI(t, root, "authoring", "capabilities")
	if err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	var caps map[string]any
	if json.Unmarshal(capability, &caps) != nil || caps["schema_version"] != "yawr.authoring-capabilities/v1" || caps["max_items"] != float64(4096) {
		t.Fatal("bad capabilities")
	}
}

func authoringWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := testworkspace.Claim(root, "cmd/yawr"); err != nil {
		t.Fatal(err)
	}
	return root
}
