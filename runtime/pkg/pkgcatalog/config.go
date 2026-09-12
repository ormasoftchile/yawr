package pkgcatalog

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

type BindingOrigin struct {
	Package string
	Origin  string
}

func MergePackageBindings(project, override []*schema.PackageRequirement) ([]*schema.PackageRequirement, []BindingOrigin) {
	byName := map[string]*schema.PackageRequirement{}
	origins := map[string]string{}
	order := []string{}
	for i, group := range [][]*schema.PackageRequirement{project, override} {
		for _, r := range group {
			if r == nil {
				continue
			}
			if _, ok := byName[r.Package]; !ok {
				order = append(order, r.Package)
			}
			cp := *r
			byName[r.Package] = &cp
			origins[r.Package] = "project"
			if i == 1 {
				origins[r.Package] = "package-map"
			}
		}
	}
	merged := make([]*schema.PackageRequirement, 0, len(order))
	provenance := make([]BindingOrigin, 0, len(order))
	for _, name := range order {
		merged = append(merged, byName[name])
		provenance = append(provenance, BindingOrigin{Package: name, Origin: origins[name]})
	}
	return merged, provenance
}
