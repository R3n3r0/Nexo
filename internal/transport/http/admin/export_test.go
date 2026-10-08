package admin

import "github.com/r3n3r0/nexo/internal/service/policy"

// RouteSpec describes a registered route for the contract tests.
type RouteSpec struct {
	Method    string
	Pattern   string
	Actions   []policy.Action
	ByService bool
}

// RouteSpecs returns the route table of the groups (public routes are not
// part of the tables).
func RouteSpecs(groups ...Routes) []RouteSpec {
	var out []RouteSpec
	for _, g := range groups {
		t, ok := g.(routeTable)
		if !ok {
			continue
		}
		for _, rt := range t.routes() {
			out = append(out, RouteSpec{Method: rt.method, Pattern: rt.pattern, Actions: rt.req.declared(), ByService: rt.req.byService && rt.req.extra == nil})
		}
	}
	return out
}
