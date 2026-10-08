package policy

import (
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// allS3Resources matches every bucket and object.
const allS3Resources = "arn:aws:s3:::*"

// builtInPolicies returns the policies shipped with Nexo. They are built on
// demand (no package-level state) and marked read-only.
func builtInPolicies(at time.Time) map[string]Named {
	mk := func(name string, statements ...domain.Statement) Named {
		return Named{Name: name, Policy: &domain.Policy{Version: domain.PolicyVersion, Statement: statements}, UpdatedAt: at, BuiltIn: true}
	}
	return map[string]Named{
		PolicyConsoleAdmin: mk(PolicyConsoleAdmin,
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionAdminAll)}, Resource: domain.StringSet{"*"}},
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionAll)}, Resource: domain.StringSet{allS3Resources}},
		),
		PolicyReadOnly: mk(PolicyReadOnly,
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionGetBucketLocation), string(ActionGetObject)}, Resource: domain.StringSet{allS3Resources}},
		),
		PolicyReadWrite: mk(PolicyReadWrite,
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionAll)}, Resource: domain.StringSet{allS3Resources}},
		),
		PolicyWriteOnly: mk(PolicyWriteOnly,
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionPutObject)}, Resource: domain.StringSet{allS3Resources}},
		),
		PolicyDiagnostics: mk(PolicyDiagnostics,
			domain.Statement{Effect: domain.Allow, Action: domain.StringSet{string(ActionAdminServerInfo), "admin:Prometheus", "admin:Health"}, Resource: domain.StringSet{"*"}},
		),
	}
}
