package policy

import (
	"sort"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// builtInPolicies returns the policies shipped with Nexo (docs/09 §9.3).
// They are built on demand (no package-level state), read-only and never
// stored:
//
//   - readonly: read and list everything (s3:Get*, s3:List*), like the AWS
//     managed AmazonS3ReadOnlyAccess, so that read-only users can browse
//     in the console;
//   - readwrite: every S3 action (s3:*), like MinIO;
//   - writeonly: upload only (s3:PutObject and the multipart actions,
//     s3:GetBucketLocation for the SDKs), no read, list or delete;
//   - diagnostics: the MinIO diagnostics admin actions (server info,
//     trace, logs, profiling, speed tests, locks, OBD, bandwidth,
//     Prometheus, health);
//   - consoleAdmin: every admin, KMS and S3 action, like MinIO.
func builtInPolicies(at time.Time) map[string]Named {
	allow := func(resource string, actions ...string) domain.Statement {
		return domain.Statement{Effect: domain.Allow, Action: actions, Resource: domain.StringSet{resource}}
	}
	mk := func(name string, statements ...domain.Statement) Named {
		return Named{Name: name, Policy: &domain.Policy{Version: domain.PolicyVersion, Statement: statements}, UpdatedAt: at, BuiltIn: true}
	}
	return map[string]Named{
		PolicyConsoleAdmin: mk(PolicyConsoleAdmin,
			allow(allS3Resources, string(ActionAdminAll), "kms:*"),
			allow(allS3Resources, string(ActionAll)),
		),
		PolicyReadOnly: mk(PolicyReadOnly,
			allow(allS3Resources, "s3:Get*", "s3:List*"),
		),
		PolicyReadWrite: mk(PolicyReadWrite,
			allow(allS3Resources, string(ActionAll)),
		),
		PolicyWriteOnly: mk(PolicyWriteOnly,
			allow(allS3Resources, string(ActionGetBucketLocation), string(ActionPutObject), string(ActionAbortMultipart),
				string(ActionListMultipartParts), string(ActionListMultipart)),
		),
		PolicyDiagnostics: mk(PolicyDiagnostics,
			allow(allS3Resources, string(ActionAdminServerInfo), "admin:ServerTrace", "admin:ConsoleLog", string(ActionAdminProfiling),
				string(ActionAdminSpeedtest), "admin:TopLocksInfo", "admin:OBDInfo", "admin:BandwidthMonitor", string(ActionAdminPrometheus),
				"admin:Health"),
		),
	}
}

// sortedBuiltins returns the built-in policies sorted by name.
func sortedBuiltins(m map[string]Named) []Named {
	out := make([]Named, 0, len(m))
	for _, n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
