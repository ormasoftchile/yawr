package tool

import (
	"errors"
	"strings"
	"unicode/utf16"

	runtimetool "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"gopkg.in/yaml.v3"
)

// AuthoringMetadata deliberately has no slot for default values or transports.
type AuthoringMetadata struct {
	Description string
	Actions     map[string]AuthoringAction
}
type AuthoringAction struct {
	Description string
	Arguments   map[string]AuthoringArgument
}
type AuthoringArgument struct {
	Type, Description    string
	Required, HasDefault bool
}

func authoringDescription(s string) string {
	if len(utf16.Encode([]rune(s))) > 2048 {
		return ""
	}
	return s
}
func sensitiveArgument(name, typ string) bool {
	s := strings.ToLower(name)
	s = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(s)
	if strings.Contains(strings.ToLower(typ), "secret") {
		return true
	}
	for _, word := range []string{"password", "passwd", "secret", "token", "credential", "apikey", "privatekey", "accesskey", "connectionstring", "authorization", "authheader", "signingkey"} {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}
func metadataValue(n *yaml.Node, key string) *yaml.Node {
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
	}
	return nil
}
func metadataString(n *yaml.Node) string {
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		return n.Value
	}
	return ""
}

// AuthoringProjection consumes exactly the snapshot already bound by BindFile,
// checking the filtered identity again before projecting authored documentation.
func AuthoringProjection(data []byte, bound runtimetool.ToolDef) (AuthoringMetadata, error) {
	out := AuthoringMetadata{Actions: map[string]AuthoringAction{}}
	if bound.SourcePath == "" {
		for name, a := range bound.Actions {
			if a == nil {
				continue
			}
			v := AuthoringAction{Description: authoringDescription(a.Description), Arguments: map[string]AuthoringArgument{}}
			for name, arg := range a.Args {
				if arg == nil {
					continue
				}
				d := authoringDescription(arg.Description)
				if sensitiveArgument(name, arg.Type) {
					d = ""
				}
				v.Arguments[name] = AuthoringArgument{Type: arg.Type, Description: d, Required: arg.Required, HasDefault: arg.Default != nil}
			}
			out.Actions[name] = v
		}
		return out, nil
	}
	identity, err := parsePresentationMetadata(data)
	if err != nil || identity.Name == "" || bound.Source != "tool://"+identity.Name || len(identity.Actions) != len(bound.Actions) {
		return out, errors.New("incomplete-identity")
	}

	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) != 1 {
		return out, errors.New("incomplete-source")
	}
	root := doc.Content[0]
	out.Description = authoringDescription(metadataString(metadataValue(root, "description")))
	if meta := metadataValue(root, "meta"); meta != nil {
		if d := metadataString(metadataValue(meta, "description")); d != "" {
			out.Description = authoringDescription(d)
		}
	}
	actions := metadataValue(root, "actions")
	if actions == nil {
		return out, errors.New("incomplete-identity")
	}
	for i := 0; i < len(actions.Content); i++ {
		a := actions.Content[i]
		name := metadataString(metadataValue(a, "name"))
		if actions.Kind == yaml.MappingNode {
			name = a.Value
			i++
			a = actions.Content[i]
		}
		b, ok := bound.Actions[name]
		if !ok || b == nil {
			return out, errors.New("incomplete-identity")
		}
		v := AuthoringAction{Description: authoringDescription(metadataString(metadataValue(a, "description"))), Arguments: map[string]AuthoringArgument{}}
		args := metadataValue(a, "args")
		if args != nil && args.Kind != yaml.MappingNode {
			return out, errors.New("incomplete-identity")
		}
		if args != nil {
			for j := 0; j < len(args.Content); j += 2 {
				name, arg := args.Content[j].Value, args.Content[j+1]
				typ := metadataString(metadataValue(arg, "type"))
				bArg, ok := b.Args[name]
				if !ok || bArg == nil || bArg.Type != typ {
					return out, errors.New("incomplete-identity")
				}
				required := false
				if r := metadataValue(arg, "required"); r != nil && r.Decode(&required) != nil {
					return out, errors.New("incomplete-identity")
				}
				description := authoringDescription(metadataString(metadataValue(arg, "description")))
				if sensitiveArgument(name, typ) {
					description = ""
				}
				v.Arguments[name] = AuthoringArgument{Type: typ, Description: description, Required: required, HasDefault: metadataValue(arg, "default") != nil}
			}
		}
		if len(v.Arguments) != len(b.Args) {
			return out, errors.New("incomplete-identity")
		}
		out.Actions[name] = v
	}
	return out, nil
}

func AuthoringIdentity(data []byte) (string, error) {
	def, err := parsePresentationMetadata(data)
	if err != nil {
		return "", err
	}
	return def.Name, nil
}
