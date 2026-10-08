package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newAdminPolicyCmd(e *env) *cobra.Command {
	cmd := groupCmd("policy", "Manage the IAM policies (AWS policy language)")
	cmd.Long = `Manage the named IAM policies, attach them to users and groups and simulate their
effect. The built-in policies are readonly, readwrite, writeonly, diagnostics and
consoleAdmin.`
	cmd.AddCommand(
		newAdminPolicyCreateCmd(e), newAdminPolicyRemoveCmd(e), newAdminPolicyListCmd(e), newAdminPolicyInfoCmd(e),
		newAdminPolicyAttachCmd(e, true), newAdminPolicyAttachCmd(e, false), newAdminPolicyEntitiesCmd(e),
		newAdminPolicySimulateCmd(e), newAdminPolicyValidateCmd(e),
	)
	return cmd
}

func newAdminPolicyCreateCmd(e *env) *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "create [flags] ALIAS NAME FILE",
		Short: "Create a policy from a JSON document (FILE, or - for stdin)",
		Long: `Create a named policy from a JSON policy document. A malformed document is
rejected with the line and the column of each error. --update replaces the
document of an existing policy (or creates it).`,
		Example: `  nexoctl admin policy create local photos-ro photos-ro.json
  cat policy.json | nexoctl admin policy create --update local photos-ro -`,
		Args:              exactArgs(3),
		ValidArgsFunction: completeAdminPolicyFile(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			doc, err := e.readDocument(args[2])
			if err != nil {
				return err
			}
			var p *nexoadmin.Policy
			if update {
				p, err = client.SavePolicy(ctx, args[1], doc)
			} else {
				p, err = client.CreatePolicy(ctx, args[1], doc)
			}
			if err != nil {
				return fmt.Errorf("%s: policy %q: %w", args[2], args[1], err)
			}
			verb := "Created"
			if update {
				verb = "Saved"
			}
			return e.out.Info(iamPolicyRecord(alias, p, false), fmt.Sprintf("%s policy %q.", verb, p.Name))
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "replace the document when the policy exists")
	return cmd
}

// iamPolicyRecord converts a policy; the document only when withDocument.
func iamPolicyRecord(alias string, p *nexoadmin.Policy, withDocument bool) adminPolicyRecord {
	r := adminPolicyRecord{Alias: alias, Name: p.Name, BuiltIn: p.BuiltIn, UpdatedAt: timePtr(p.UpdatedAt)}
	if withDocument {
		r.Document = p.Document
	}
	return r
}

func newAdminPolicyRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS NAME...",
		Aliases:           []string{"rm"},
		Short:             "Remove policies (built-in policies cannot be removed)",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, policyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				if err := client.DeletePolicy(ctx, name); err != nil {
					return fmt.Errorf("remove policy %q: %w", name, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindPolicy, Name: name, Action: "removed"},
					fmt.Sprintf("Removed policy %q.", name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminPolicyListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the policies",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			policies, err := listPolicies(ctx, client)
			if err != nil {
				return err
			}
			if len(policies) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-24s %-8s %s", "POLICY", "TYPE", "UPDATED")); err != nil {
					return err
				}
			}
			for i := range policies {
				p := &policies[i]
				kind := "custom"
				if p.BuiltIn {
					kind = "built-in"
				}
				text := fmt.Sprintf("%s %-8s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-24s", p.Name)), kind, cli.FormatTime(p.UpdatedAt))
				if err := e.out.Result(iamPolicyRecord(alias, p, false), strings.TrimRight(text, " ")); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminPolicyInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS NAME",
		Short:             "Print the document of a policy",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, policyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			p, err := client.GetPolicy(ctx, args[1])
			if err != nil {
				return fmt.Errorf("policy %q: %w", args[1], err)
			}
			return e.out.Result(iamPolicyRecord(alias, p, true), indentJSON(p.Document))
		},
	}
}

// indentJSON pretty-prints a JSON document (unchanged when invalid).
func indentJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String()
}

func newAdminPolicyAttachCmd(e *env, attach bool) *cobra.Command {
	var user, group string
	verb, done, prep := "attach", "attached", "to"
	if !attach {
		verb, done, prep = "detach", "detached", "from"
	}
	cmd := &cobra.Command{
		Use:               verb + " [flags] ALIAS POLICY... (--user USER | --group GROUP)",
		Short:             capitalize(verb) + " policies " + prep + " a user or a group",
		Example:           fmt.Sprintf("  nexoctl admin policy %s local readonly --user alice", verb),
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, policyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (user == "") == (group == "") {
				return cli.UsageError("exactly one of --user and --group is required")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			in := nexoadmin.AttachRequest{PolicyNames: args[1:], User: user, Group: group}
			call := client.AttachPolicy
			if !attach {
				call = client.DetachPolicy
			}
			target, kind := "user "+user, kindUser
			if group != "" {
				target, kind = "group "+group, kindGroup
			}
			if err := call(ctx, in); err != nil {
				return fmt.Errorf("%s %s %s %s: %w", verb, strings.Join(args[1:], ","), prep, target, err)
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: kind, Name: user + group, Action: done, Names: args[1:]},
				fmt.Sprintf("Policies %s %s %s %q.", strings.Join(args[1:], ", "), done, prep, user+group))
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "the user")
	cmd.Flags().StringVar(&group, "group", "", "the group")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	_ = cmd.RegisterFlagCompletionFunc("group", completeAdminFlag(e, groupNames))
	return cmd
}

func newAdminPolicyEntitiesCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "entities ALIAS POLICY",
		Short:             "List the users and groups a policy is attached to",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, policyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			ent, err := client.PolicyEntities(ctx, args[1])
			if err != nil {
				return fmt.Errorf("policy %q: %w", args[1], err)
			}
			rec := adminEntitiesRecord{Alias: alias, Policy: ent.Policy, Users: names(ent.Users), Groups: names(ent.Groups)}
			text := fmt.Sprintf("Policy: %s\nUsers: %s\nGroups: %s", ent.Policy, joinOrDash(ent.Users), joinOrDash(ent.Groups))
			return e.out.Result(rec, text)
		},
	}
}

func newAdminPolicySimulateCmd(e *env) *cobra.Command {
	var (
		user, action, sourceIP, at string
		secure, insecure, verbose  bool
		conditions                 []string
	)
	cmd := &cobra.Command{
		Use:   "simulate [flags] ALIAS[/BUCKET[/OBJECT]] --action ACTION",
		Short: "Simulate a request: allowed or denied, and by which statement",
		Long: `Evaluate a request with the policy engine of the server, as the S3 API would
(identity policies, group policies, inline policies of access keys and the
bucket policy), and print the decision with the statement that decided.
The request is made by --user (default: the user of the alias).`,
		Example: `  nexoctl admin policy simulate --user alice --action s3:GetObject local/photos/2026/a.jpg
  nexoctl admin policy simulate --action s3:ListBucket --condition s3:prefix=2026/ local/photos`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if action == "" {
				return cli.UsageError("--action is required (e.g. s3:GetObject)")
			}
			if secure && insecure {
				return cli.UsageError("--secure excludes --insecure")
			}
			in := nexoadmin.SimulateRequest{User: user, Action: action, SourceIP: sourceIP}
			if secure || insecure {
				in.SecureTransport = &secure
			}
			if at != "" {
				ts, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return cli.UsageError("--time %q: use RFC 3339 (2026-01-02T15:04:05Z)", at)
				}
				in.Time = &ts
			}
			for _, c := range conditions {
				k, v, ok := strings.Cut(c, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return cli.UsageError("invalid condition %q (use key=value)", c)
				}
				if in.Conditions == nil {
					in.Conditions = map[string][]string{}
				}
				k = strings.TrimSpace(k)
				in.Conditions[k] = append(in.Conditions[k], v)
			}
			ctx := cmd.Context()
			t, err := e.remoteTarget(ctx, args[0], false)
			if err != nil {
				return err
			}
			in.Bucket, in.Object = t.Bucket, t.Key
			client, err := e.admin(ctx, t.Alias)
			if err != nil {
				return err
			}
			sim, err := client.SimulatePolicy(ctx, in)
			if err != nil {
				return fmt.Errorf("simulate %s on %s: %w", action, t, err)
			}
			return e.out.Result(adminSimulationRecord{Alias: t.Alias, Simulation: *sim}, e.formatSimulation(t, sim, verbose))
		},
	}
	f := cmd.Flags()
	f.StringVar(&action, "action", "", "the action (s3:GetObject, s3:PutObject, admin:CreateUser, ...)")
	f.StringVar(&user, "user", "", "the user making the request (default: the user of the alias)")
	f.StringVar(&sourceIP, "source-ip", "", "aws:SourceIp of the request")
	f.BoolVar(&secure, "secure", false, "the request uses TLS (aws:SecureTransport=true)")
	f.BoolVar(&insecure, "insecure", false, "the request does not use TLS (aws:SecureTransport=false)")
	f.StringVar(&at, "time", "", "aws:CurrentTime of the request (RFC 3339)")
	f.StringArrayVar(&conditions, "condition", nil, "condition key of the request, key=value (repeatable)")
	f.BoolVarP(&verbose, "verbose", "v", false, "list every evaluated statement")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	return cmd
}

// formatSimulation renders a simulation.
func (e *env) formatSimulation(t cli.Target, sim *nexoadmin.Simulation, verbose bool) string {
	var b strings.Builder
	decision := e.out.Paint(cli.ColorGreen, sim.Decision)
	if !sim.Allowed {
		decision = e.out.Paint(cli.ColorRed, sim.Decision)
	}
	resource := "*"
	if t.Bucket != "" {
		resource = t.Bucket
		if t.Key != "" {
			resource += "/" + t.Key
		}
	}
	fmt.Fprintf(&b, "Decision: %s\nUser: %s\nAction: %s\nResource: %s\nReason: %s", decision, sim.User, sim.Action, resource, orDash(sim.Reason))
	if sim.Statement != nil {
		fmt.Fprintf(&b, "\nStatement: %s", describeStatement(*sim.Statement))
	}
	if verbose {
		for _, st := range sim.Evaluated {
			mark := "-"
			if st.Matched {
				mark = "+"
			}
			line := fmt.Sprintf("\n  %s %s", mark, describeStatement(st))
			if st.Mismatch != "" {
				line += " (" + st.Mismatch + ")"
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

// describeStatement names a statement: "Allow, statement 0 (Sid) of
// policy readwrite (group devs)".
func describeStatement(st nexoadmin.EvaluatedStatement) string {
	s := fmt.Sprintf("%s, statement %d", st.Effect, st.Index+1)
	if st.Sid != "" {
		s += fmt.Sprintf(" (%s)", st.Sid)
	}
	switch {
	case st.PolicyName != "":
		s += " of policy " + st.PolicyName
	case st.Source != "":
		s += " of the " + st.Source + " policy"
	}
	if st.Group != "" {
		s += " (group " + st.Group + ")"
	}
	return s
}

func newAdminPolicyValidateCmd(e *env) *cobra.Command {
	var bucket string
	cmd := &cobra.Command{
		Use:   "validate [flags] ALIAS FILE",
		Short: "Validate a policy document without saving it",
		Long: `Validate an IAM policy document (or with --bucket a bucket policy) with the
server: the errors are printed with their line and column, and the command
fails when the document is invalid.`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdminPolicyFile(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			doc, err := e.readDocument(args[1])
			if err != nil {
				return err
			}
			rep, err := client.ValidatePolicy(ctx, doc, bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", args[1], err)
			}
			rec := adminValidationRecord{Alias: alias, File: args[1], Bucket: bucket, Valid: rep.Valid, Errors: []cli.ErrorDetail{}}
			for _, pe := range rep.Errors {
				rec.Errors = append(rec.Errors, cli.ErrorDetail{
					Statement: pe.Statement, Sid: pe.Sid, Element: pe.Element, Line: pe.Line, Column: pe.Column, Message: pe.Message,
				})
			}
			if !rep.Valid {
				return &documentError{file: args[1], what: "policy", sentinel: domain.ErrMalformedPolicy, details: rec.Errors}
			}
			return e.out.Result(rec, fmt.Sprintf("%s: valid policy.", args[1]))
		},
	}
	cmd.Flags().StringVar(&bucket, "bucket", "", "validate a bucket policy of this bucket")
	return cmd
}

// documentError reports an invalid document (admin policy validate, a
// local file that is not JSON): it wraps its sentinel
// (domain.ErrMalformedPolicy, domain.ErrInvalidArgument) and carries the
// located errors.
type documentError struct {
	file     string
	what     string
	sentinel error
	details  []cli.ErrorDetail
}

func (e *documentError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: invalid %s", e.file, e.what)
	for _, d := range e.details {
		b.WriteString("\n  " + d.String())
	}
	return b.String()
}

func (e *documentError) Unwrap() error { return e.sentinel }

// Details returns the located errors.
func (e *documentError) Details() []cli.ErrorDetail { return e.details }

// completeAdminPolicyFile completes ALIAS NAME FILE / ALIAS FILE: the
// alias, then local files.
func completeAdminPolicyFile(e *env) cobra.CompletionFunc {
	aliases := completeAliasNames(e)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return aliases(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveDefault
	}
}
