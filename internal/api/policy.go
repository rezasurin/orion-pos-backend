package api

import (
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
)

// policy is what an operation requires of its caller, read from the OpenAPI document so the
// contract and the enforcement cannot drift apart.
type policy struct {
	operator   bool              // operatorAuth: an Orion operator, not a tenant principal
	public     bool              // security: [] in the spec
	audience   identity.Audience // the token family the route accepts
	permission string            // x-permission, empty if any signed-in caller will do
}

// policiesFromSpec reads each operation's `security` and `x-permission`. It refuses anything it
// does not understand, so a typo in the spec stops the server from starting instead of leaving
// a route open.
func policiesFromSpec(doc *openapi3.T) (map[string]policy, error) {
	out := map[string]policy{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			id := op.OperationID
			if id == "" {
				return nil, fmt.Errorf("%s %s has no operationId", method, path)
			}
			sec := doc.Security
			if op.Security != nil {
				sec = *op.Security
			}

			var p policy
			switch {
			case len(sec) == 0:
				p.public = true
			case len(sec) == 1 && len(sec[0]) == 1:
				for scheme := range sec[0] {
					switch scheme {
					case "userAuth":
						p.audience = identity.AudienceTenant
					case "operatorAuth":
						p.operator = true
					case "deviceAuth":
						p.audience = identity.AudienceDevice
					default:
						return nil, fmt.Errorf("%s: unknown security scheme %q", id, scheme)
					}
				}
			default:
				return nil, fmt.Errorf("%s: exactly one security scheme is supported per operation", id)
			}

			if v, ok := op.Extensions["x-permission"]; ok {
				perm, isString := v.(string)
				if !isString || perm == "" {
					return nil, fmt.Errorf("%s: x-permission must be a non-empty string", id)
				}
				if p.public {
					return nil, fmt.Errorf("%s: x-permission on a public operation", id)
				}
				if !identity.IsPermission(perm) {
					return nil, fmt.Errorf("%s: x-permission %q is not a known permission", id, perm)
				}
				p.permission = perm
			}
			if _, dup := out[id]; dup {
				return nil, fmt.Errorf("duplicate operationId %q", id)
			}
			out[id] = p
		}
	}
	return out, nil
}
