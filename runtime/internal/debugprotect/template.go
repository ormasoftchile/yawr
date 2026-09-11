package debugprotect

import (
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

// TemplateVariables returns runtime variable roots referenced by a GIS
// template. all is true when the expression references the complete vars map.
func TemplateVariables(template string) (names []string, all bool, err error) {
	parsed, err := gis.Parse(template)
	if err != nil {
		return nil, false, err
	}
	references := make(map[string]bool)
	for _, segment := range parsed.Segments {
		expression, ok := segment.(*gis.Expr)
		if !ok || expression.Parsed == nil {
			continue
		}
		collectTemplateVariables(expression.Parsed.Node, references, &all)
	}
	names = make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, all, nil
}

func collectTemplateVariables(node gxlparser.Node, names map[string]bool, all *bool) {
	switch typed := node.(type) {
	case *gxlparser.PathRef:
		if typed.Root != "vars" {
			names[typed.Root] = true
			return
		}
		if len(typed.Segments) == 0 || typed.Segments[0].Name == "" {
			*all = true
			return
		}
		names[typed.Segments[0].Name] = true
	case *gxlparser.BinaryOp:
		collectTemplateVariables(typed.Left, names, all)
		collectTemplateVariables(typed.Right, names, all)
	case *gxlparser.UnaryOp:
		collectTemplateVariables(typed.Expr, names, all)
	case *gxlparser.Call:
		for _, argument := range typed.Args {
			collectTemplateVariables(argument, names, all)
		}
	case *gxlparser.Group:
		collectTemplateVariables(typed.Expr, names, all)
	}
}
