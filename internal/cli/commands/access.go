package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// newAccessCmd returns access: the access rules of the buckets (docs/10
// §10.1, C-ACC-01).
func newAccessCmd(e *env) *cobra.Command {
	cmd := groupCmd("access", "Manage the access rules of the buckets (prefix, glob, regex)")
	cmd.Long = `Manage the access rules of the buckets of a Nexo server (Admin API,
admin:ManageAccessRules): rules with a priority, an effect (allow or deny),
principals, actions and a pattern of the keys (a prefix, a glob with *, **, ?,
[abc] and {a,b}, or an RE2 regular expression), evaluated by the policy engine
next to the IAM policies and the bucket policy: a deny in any source denies.

Principals: '*' (everyone, anonymous included), user:NAME, group:NAME,
accesskey:KEY, distribution:ID. Actions: read, list, write, delete, tag, all,
or S3 actions (s3:GetObject). Every change replaces the rules of the bucket
atomically and fails if they changed meanwhile.`
	rule := groupCmd("rule", "Add, list, show, remove and reorder the access rules of a bucket")
	rule.AddCommand(newAccessRuleAddCmd(e), newAccessRuleListCmd(e), newAccessRuleInfoCmd(e), newAccessRuleRemoveCmd(e), newAccessRuleMoveCmd(e))
	public := groupCmd("public", "Apply and show the public access presets of a bucket")
	public.AddCommand(newAccessPublicSetCmd(e), newAccessPublicGetCmd(e))
	block := groupCmd("block-public", "Block the public access of the cluster or of a bucket")
	block.Long = `Block the access rules granting everyone ("*"), on every bucket (ALIAS) or on
one (ALIAS/BUCKET). In the reject mode (a setting of the cluster) such a rule
is refused when saved and the stored ones are ignored; in the ignore mode they
are accepted and ignored. Deny rules are never affected.`
	block.AddCommand(newAccessBlockPublicCmd(e, true), newAccessBlockPublicCmd(e, false), newAccessBlockPublicStatusCmd(e))
	cmd.AddCommand(rule, newAccessTestCmd(e), public, block, newAccessExportCmd(e), newAccessImportCmd(e))
	return cmd
}

// accessRuleRecord is an access rule of a bucket (access rule ls|info|add).
type accessRuleRecord struct {
	Alias  string `json:"alias"`
	Bucket string `json:"bucket"`
	nexoadmin.BucketAccessRule
}

// accessRulesRecord confirms a change of the rules of a bucket.
type accessRulesRecord struct {
	Alias  string `json:"alias"`
	Bucket string `json:"bucket"`
	// Action is added, removed, moved, imported or preset.
	Action string `json:"action"`
	// ID is the rule changed (none for an import or a preset).
	ID       string   `json:"id,omitempty"`
	Rules    int      `json:"rules"`
	ETag     string   `json:"etag"`
	Public   bool     `json:"public"`
	Warnings []string `json:"warnings"`
}

// accessTestRecord is the outcome of access test for one key.
type accessTestRecord struct {
	Alias     string `json:"alias"`
	Bucket    string `json:"bucket"`
	Action    string `json:"action"`
	Principal string `json:"principal"`
	nexoadmin.AccessRulesKeyResult
}

// accessPublicRecord is the public status of a bucket.
type accessPublicRecord struct {
	Alias string `json:"alias"`
	nexoadmin.AccessRulesPublicStatus
}

// blockPublicRecord is a block public access setting.
type blockPublicRecord struct {
	Alias string `json:"alias"`
	nexoadmin.BlockPublic
}

// accessExportRecord reports an export to a file.
type accessExportRecord struct {
	Alias  string `json:"alias"`
	Bucket string `json:"bucket"`
	File   string `json:"file"`
	Rules  int    `json:"rules"`
}

// rulesAttempts bounds the read-modify-write of the rules when they
// change concurrently.
const rulesAttempts = 3

// editRules applies change to the rules of a bucket: read with their ETag,
// change, replace with If-Match, retried when the rules changed meanwhile.
func editRules(ctx context.Context, client cli.AdminClient, bucket string,
	change func(rules []nexoadmin.BucketAccessRule) ([]nexoadmin.BucketAccessRule, error)) (*nexoadmin.AccessRules, error) {
	for attempt := 1; ; attempt++ {
		cur, err := client.GetAccessRules(ctx, bucket)
		if err != nil {
			return nil, err
		}
		next, err := change(slices.Clone(cur.Rules))
		if err != nil {
			return nil, err
		}
		out, err := client.SetAccessRules(ctx, bucket, next, cur.ETag)
		if errors.Is(err, domain.ErrPreconditionFailed) && attempt < rulesAttempts {
			continue
		}
		return out, err
	}
}

// ruleIndex returns the index of the rule id, or a not-found error.
func ruleIndex(rules []nexoadmin.BucketAccessRule, id, target string) (int, error) {
	for i, r := range rules {
		if r.ID == id {
			return i, nil
		}
	}
	return -1, fmt.Errorf("access rule %q of `%s`: %w", id, target, domain.ErrNotFound)
}

// warnRules prints the warnings of the server about the rules.
func (e *env) warnRules(warnings []string) {
	for _, w := range warnings {
		e.out.Warn(w)
	}
}

// patternFlags are the flags selecting a pattern of keys or paths.
type patternFlags struct {
	prefix, glob, regex string
	ignoreCase          bool
}

func (p *patternFlags) register(cmd *cobra.Command, what string) {
	f := cmd.Flags()
	f.StringVar(&p.prefix, "prefix", "", what+": a literal prefix")
	f.StringVar(&p.glob, "glob", "", what+": a glob (*: one segment, **: any number, ?, [abc], {a,b})")
	f.StringVar(&p.regex, "regex", "", what+": an RE2 regular expression")
	f.BoolVar(&p.ignoreCase, "ignore-case", false, "match the pattern case-insensitively")
}

// pattern returns the pattern of the flags (nil without one).
func (p *patternFlags) pattern(cmd *cobra.Command) (*nexoadmin.KeyPattern, error) {
	var out *nexoadmin.KeyPattern
	for _, c := range []struct{ flag, typ, value string }{
		{"prefix", nexoadmin.PatternPrefix, p.prefix}, {"glob", nexoadmin.PatternGlob, p.glob}, {"regex", nexoadmin.PatternRegex, p.regex},
	} {
		if !cmd.Flags().Changed(c.flag) {
			continue
		}
		if out != nil {
			return nil, cli.UsageError("--prefix, --glob and --regex are mutually exclusive")
		}
		out = &nexoadmin.KeyPattern{Type: c.typ, Pattern: c.value, CaseInsensitive: p.ignoreCase}
	}
	if out == nil && p.ignoreCase {
		return nil, cli.UsageError("--ignore-case applies to a --prefix, --glob or --regex")
	}
	return out, nil
}

// formatPattern renders a pattern ("glob **/private/**", "regex(i) ^a").
func formatPattern(p *nexoadmin.KeyPattern) string {
	if p == nil {
		return "(whole bucket)"
	}
	typ := p.Type
	if p.CaseInsensitive {
		typ += "(i)"
	}
	return typ + " " + p.Pattern
}

// parseHeaderFlags parses "Name: value" flags.
func parseHeaderFlags(flag string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		name, value, ok := strings.Cut(v, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, cli.UsageError("--%s %q: expected 'Name: value'", flag, v)
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out, nil
}

// parsePairFlags parses "NAME=VALUE" flags.
func parsePairFlags(flag string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		name, value, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, cli.UsageError("--%s %q: expected NAME=VALUE", flag, v)
		}
		out[strings.TrimSpace(name)] = value
	}
	return out, nil
}

// defaultRulePriority is the priority of a rule added without --priority.
const defaultRulePriority = 100

func newAccessRuleAddCmd(e *env) *cobra.Command {
	var (
		id, description, secure, from, to string
		allow, deny, replace              bool
		priority                          int
		principals, actions               []string
		sourceIP, notSourceIP, referer    []string
		hosts, methods, headers, captures []string
		pf                                patternFlags
	)
	cmd := &cobra.Command{
		Use:   "add ALIAS/BUCKET --id ID (--allow|--deny) --principal P --action A [--prefix|--glob|--regex PATTERN]",
		Short: "Add an access rule to a bucket",
		Long: `Add an access rule to a bucket. Without --prefix, --glob or --regex the rule covers
the whole bucket. The conditions all must hold: --source-ip/--not-source-ip
(CIDRs, the client behind the trusted proxies), --referer (patterns with * and
?), --host (the host of the request, *.example.com for the subdomains),
--secure-transport, --from/--to (RFC 3339), --method, --header 'Name: value'
(the value may be ${secret:NAME}) and --capture NAME=VALUE (a named group of
the regex, e.g. user='${aws:username}').`,
		Example: `  nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
      --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
  nexoctl access rule add local/media --id no-private --deny --principal '*' --action all --glob '**/private/**' --priority 5
  nexoctl access rule add local/home --id home --allow --principal 'group:staff' --action all \
      --regex '^home/(?P<user>[^/]+)/' --capture 'user=${aws:username}'`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if allow == deny {
				return cli.UsageError("one of --allow and --deny is required")
			}
			if strings.TrimSpace(id) == "" {
				return cli.UsageError("--id is required")
			}
			match, err := pf.pattern(cmd)
			if err != nil {
				return err
			}
			rule := nexoadmin.BucketAccessRule{
				ID: id, Priority: priority, Effect: nexoadmin.EffectAllow, Principals: splitList(principals), Actions: splitList(actions),
				Match: match, Description: description,
			}
			if deny {
				rule.Effect = nexoadmin.EffectDeny
			}
			if len(rule.Principals) == 0 || len(rule.Actions) == 0 {
				return cli.UsageError("--principal and --action are required")
			}
			if rule.Conditions, err = ruleConditions(cmd, sourceIP, notSourceIP, referer, hosts, methods, headers, captures, secure, from, to); err != nil {
				return err
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access rule add")
			if err != nil {
				return err
			}
			out, err := editRules(ctx, client, t.Bucket, func(rules []nexoadmin.BucketAccessRule) ([]nexoadmin.BucketAccessRule, error) {
				if i, err := ruleIndex(rules, id, t.String()); err == nil {
					if !replace {
						return nil, fmt.Errorf("%w: the access rule %q exists in `%s` (use --replace)", domain.ErrConflict, id, t)
					}
					rules[i] = rule
					return rules, nil
				}
				return append(rules, rule), nil
			})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			e.warnRules(out.Warnings)
			return e.out.Info(rulesRecordOf(t, "added", id, out), fmt.Sprintf("Added the access rule `%s` to `%s` (%d %s).", id, t, len(out.Rules), plural(len(out.Rules), "rule", "rules")))
		},
	}
	f := cmd.Flags()
	f.StringVar(&id, "id", "", "id of the rule (letters, digits, '.', '_', '-')")
	f.BoolVar(&allow, "allow", false, "allow the actions")
	f.BoolVar(&deny, "deny", false, "deny the actions (a deny in any source denies)")
	f.StringArrayVar(&principals, "principal", nil, "principal: '*', user:NAME, group:NAME, accesskey:KEY, distribution:ID (repeatable)")
	f.StringArrayVar(&actions, "action", nil, "action: read, list, write, delete, tag, all or an S3 action (repeatable)")
	f.IntVar(&priority, "priority", defaultRulePriority, "order of evaluation, lower first")
	f.StringVar(&description, "description", "", "free text")
	f.BoolVar(&replace, "replace", false, "replace the rule with the same id")
	pf.register(cmd, "keys of the rule")
	f.StringArrayVar(&sourceIP, "source-ip", nil, "condition: the client is in this CIDR (repeatable)")
	f.StringArrayVar(&notSourceIP, "not-source-ip", nil, "condition: the client is not in this CIDR (repeatable)")
	f.StringArrayVar(&referer, "referer", nil, "condition: the Referer matches this pattern (repeatable)")
	f.StringArrayVar(&hosts, "host", nil, "condition: the request is addressed to this host (repeatable)")
	f.StringVar(&secure, "secure-transport", "", "condition: the request uses HTTPS (true) or not (false)")
	f.StringArrayVar(&methods, "method", nil, "condition: the HTTP method (repeatable)")
	f.StringArrayVar(&headers, "header", nil, "condition: the request header 'Name: value' (repeatable)")
	f.StringArrayVar(&captures, "capture", nil, "condition: the named group NAME of the regex equals VALUE (repeatable)")
	f.StringVar(&from, "from", "", "condition: not before this time (RFC 3339)")
	f.StringVar(&to, "to", "", "condition: not after this time (RFC 3339)")
	return cmd
}

// ruleConditions builds the conditions of the flags (nil without any).
func ruleConditions(cmd *cobra.Command, sourceIP, notSourceIP, referer, hosts, methods, headers, captures []string, secure, from, to string) (*nexoadmin.AccessRuleConditions, error) {
	c := &nexoadmin.AccessRuleConditions{
		SourceIP: splitList(sourceIP), NotSourceIP: splitList(notSourceIP), Referer: referer, Host: splitList(hosts), Methods: splitList(methods),
	}
	var err error
	if c.Headers, err = parseHeaderFlags("header", headers); err != nil {
		return nil, err
	}
	if c.CaptureEquals, err = parsePairFlags("capture", captures); err != nil {
		return nil, err
	}
	if cmd.Flags().Changed("secure-transport") {
		b, err := strconv.ParseBool(secure)
		if err != nil {
			return nil, cli.UsageError("--secure-transport must be true or false")
		}
		c.SecureTransport = &b
	}
	if from != "" || to != "" {
		c.TimeWindow = &nexoadmin.TimeWindow{From: from, To: to}
	}
	empty := nexoadmin.AccessRuleConditions{}
	b1, _ := json.Marshal(c)
	b2, _ := json.Marshal(empty)
	if bytes.Equal(b1, b2) {
		return nil, nil
	}
	return c, nil
}

// rulesRecordOf builds the record of a change of the rules.
func rulesRecordOf(t cli.Target, action, id string, out *nexoadmin.AccessRules) accessRulesRecord {
	w := out.Warnings
	if w == nil {
		w = []string{}
	}
	return accessRulesRecord{Alias: t.Alias, Bucket: t.Bucket, Action: action, ID: id, Rules: len(out.Rules), ETag: out.ETag, Public: out.Public, Warnings: w}
}

// evaluationOrder sorts the rules as the engine evaluates them: deny rules
// first, then allow rules, each by priority, ties in the order of the
// document.
func evaluationOrder(rules []nexoadmin.BucketAccessRule) []nexoadmin.BucketAccessRule {
	out := slices.Clone(rules)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := out[i].Effect == nexoadmin.EffectDeny, out[j].Effect == nexoadmin.EffectDeny
		if di != dj {
			return di
		}
		return out[i].Priority < out[j].Priority
	})
	return out
}

func newAccessRuleListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS/BUCKET",
		Aliases:           []string{"ls"},
		Short:             "List the access rules of a bucket, in evaluation order",
		Long:              "List the access rules of a bucket in the order the policy engine evaluates them: the deny rules first, then the allow rules, each by priority.",
		Example:           "  nexoctl access rule ls local/media",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access rule ls")
			if err != nil {
				return err
			}
			cur, err := client.GetAccessRules(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			if len(cur.Rules) == 0 {
				return e.out.Note(fmt.Sprintf("No access rule on `%s`.", t))
			}
			if err := e.out.Text(fmt.Sprintf("%-20s %8s  %-6s %-20s %-16s %s", "ID", "PRIORITY", "EFFECT", "PRINCIPALS", "ACTIONS", "MATCH")); err != nil {
				return err
			}
			for _, r := range evaluationOrder(cur.Rules) {
				text := fmt.Sprintf("%-20s %8d  %-6s %-20s %-16s %s", r.ID, r.Priority, r.Effect, strings.Join(r.Principals, ","),
					strings.Join(r.Actions, ","), formatPattern(r.Match))
				if r.Conditions != nil {
					text += " (conditions)"
				}
				if err := e.out.Result(accessRuleRecord{Alias: t.Alias, Bucket: t.Bucket, BucketAccessRule: r}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAccessRuleInfoCmd(e *env) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:               "info ALIAS/BUCKET --id ID",
		Short:             "Show an access rule",
		Example:           "  nexoctl access rule info local/media --id img-pub",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return cli.UsageError("--id is required")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access rule info")
			if err != nil {
				return err
			}
			cur, err := client.GetAccessRules(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			i, err := ruleIndex(cur.Rules, id, t.String())
			if err != nil {
				return err
			}
			r := cur.Rules[i]
			return e.out.Result(accessRuleRecord{Alias: t.Alias, Bucket: t.Bucket, BucketAccessRule: r}, formatRule(r))
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "id of the rule")
	return cmd
}

// formatRule renders an access rule with its conditions.
func formatRule(r nexoadmin.BucketAccessRule) string {
	lines := []string{
		"ID: " + r.ID,
		"Priority: " + strconv.Itoa(r.Priority),
		"Effect: " + r.Effect,
		"Principals: " + strings.Join(r.Principals, ", "),
		"Actions: " + strings.Join(r.Actions, ", "),
		"Match: " + formatPattern(r.Match),
	}
	if r.Description != "" {
		lines = append(lines, "Description: "+r.Description)
	}
	if c := r.Conditions; c != nil {
		add := func(name string, values []string) {
			if len(values) > 0 {
				lines = append(lines, fmt.Sprintf("Condition %s: %s", name, strings.Join(values, ", ")))
			}
		}
		add("sourceIp", c.SourceIP)
		add("notSourceIp", c.NotSourceIP)
		add("referer", c.Referer)
		add("host", c.Host)
		add("methods", c.Methods)
		if c.SecureTransport != nil {
			lines = append(lines, fmt.Sprintf("Condition secureTransport: %t", *c.SecureTransport))
		}
		if c.TimeWindow != nil {
			lines = append(lines, fmt.Sprintf("Condition timeWindow: %s .. %s", orDash(c.TimeWindow.From), orDash(c.TimeWindow.To)))
		}
		for _, k := range cli.SortedKeys(c.Headers) {
			lines = append(lines, fmt.Sprintf("Condition header %s: %s", k, c.Headers[k]))
		}
		for _, k := range cli.SortedKeys(c.CaptureEquals) {
			lines = append(lines, fmt.Sprintf("Condition capture %s = %s", k, c.CaptureEquals[k]))
		}
	}
	return strings.Join(lines, "\n")
}

func newAccessRuleRemoveCmd(e *env) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:               "remove ALIAS/BUCKET --id ID",
		Aliases:           []string{"rm"},
		Short:             "Remove an access rule",
		Example:           "  nexoctl access rule rm local/media --id img-pub",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return cli.UsageError("--id is required")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access rule rm")
			if err != nil {
				return err
			}
			out, err := editRules(ctx, client, t.Bucket, func(rules []nexoadmin.BucketAccessRule) ([]nexoadmin.BucketAccessRule, error) {
				i, err := ruleIndex(rules, id, t.String())
				if err != nil {
					return nil, err
				}
				return slices.Delete(rules, i, i+1), nil
			})
			if err != nil {
				return err
			}
			return e.out.Info(rulesRecordOf(t, "removed", id, out), fmt.Sprintf("Removed the access rule `%s` from `%s`.", id, t))
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "id of the rule")
	return cmd
}

func newAccessRuleMoveCmd(e *env) *cobra.Command {
	var (
		id       string
		priority int
	)
	cmd := &cobra.Command{
		Use:               "move ALIAS/BUCKET --id ID --priority N",
		Short:             "Change the priority of an access rule",
		Example:           "  nexoctl access rule move local/media --id img-pub --priority 1",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" || !cmd.Flags().Changed("priority") {
				return cli.UsageError("--id and --priority are required")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access rule move")
			if err != nil {
				return err
			}
			out, err := editRules(ctx, client, t.Bucket, func(rules []nexoadmin.BucketAccessRule) ([]nexoadmin.BucketAccessRule, error) {
				i, err := ruleIndex(rules, id, t.String())
				if err != nil {
					return nil, err
				}
				rules[i].Priority = priority
				return rules, nil
			})
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			return e.out.Info(rulesRecordOf(t, "moved", id, out), fmt.Sprintf("Moved the access rule `%s` of `%s` to priority %d.", id, t, priority))
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "id of the rule")
	cmd.Flags().IntVar(&priority, "priority", 0, "new priority (lower first)")
	return cmd
}

func newAccessTestCmd(e *env) *cobra.Command {
	var (
		in      nexoadmin.TestAccessRulesRequest
		headers []string
		secure  string
		verbose bool
	)
	cmd := &cobra.Command{
		Use:   "test ALIAS/BUCKET/KEY... [--principal P] [--action A] [--ip IP] [--host HOST]",
		Short: "Test the access rules on keys (who may do what)",
		Long: `Evaluate requests on keys with every source of the policy engine (IAM, bucket
policy, access rules) without performing them: the rules that match, the outcome
of the rules and the final decision with the rule that determined it. The keys
are matched decoded and normalized, as the server does. The principal is
anonymous (default), user:NAME, accesskey:KEY or distribution:ID; testing another
user needs admin:SimulatePolicy. The command does not fail on a denial.`,
		Example: `  nexoctl access test local/media/img/2026/a.jpg --principal anonymous --ip 1.2.3.4 --host cdn.example.com
  nexoctl access test local/media/private/a.jpg local/media/pub/b.jpg --principal user:mario --action write -v`,
		Args:              minArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var t cli.Target
			for i, arg := range args {
				ti, err := e.remoteTarget(ctx, arg, true)
				if err != nil {
					return err
				}
				if i > 0 && (ti.Alias != t.Alias || ti.Bucket != t.Bucket) {
					return cli.UsageError("%s: every key must be in the bucket of %s", arg, t)
				}
				if i == 0 {
					t = ti
				}
				in.Keys = append(in.Keys, ti.Key)
			}
			if len(in.Keys) == 1 && in.Keys[0] == "" {
				in.Keys = nil
			}
			var err error
			if in.Headers, err = parseHeaderFlags("header", headers); err != nil {
				return err
			}
			if cmd.Flags().Changed("secure") {
				b, err := strconv.ParseBool(secure)
				if err != nil {
					return cli.UsageError("--secure must be true or false")
				}
				in.Secure = &b
			}
			client, err := e.admin(ctx, t.Alias)
			if err != nil {
				return err
			}
			res, err := client.TestAccessRules(ctx, t.Bucket, in)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			principal := in.Principal
			if principal == "" {
				principal = "anonymous"
			}
			for _, kr := range res.Results {
				rec := accessTestRecord{Alias: t.Alias, Bucket: t.Bucket, Action: res.Action, Principal: principal, AccessRulesKeyResult: kr}
				if err := e.out.Result(rec, e.formatAccessTest(t, res.Action, principal, kr, verbose)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Principal, "principal", "", "who makes the requests: anonymous, user:NAME, accesskey:KEY, distribution:ID")
	f.StringVar(&in.Action, "action", "", "action: read (default), list, write, delete, tag or an S3 action")
	f.StringVar(&in.IP, "ip", "", "address of the client")
	f.StringVar(&in.Host, "host", "", "host the requests are addressed to")
	f.StringVar(&in.Method, "method", "", "HTTP method (default: the one of the action)")
	f.StringVar(&in.Referer, "referer", "", "Referer of the requests")
	f.StringVar(&secure, "secure", "", "the requests use HTTPS (true) or not (false)")
	f.StringArrayVar(&headers, "header", nil, "request header 'Name: value' (repeatable)")
	f.StringVar(&in.Distribution, "distribution", "", "distribution serving the requests")
	f.BoolVarP(&verbose, "verbose", "v", false, "show every rule with the reason it does not apply")
	return cmd
}

// formatAccessTest renders the outcome of one key.
func (e *env) formatAccessTest(t cli.Target, action, principal string, kr nexoadmin.AccessRulesKeyResult, verbose bool) string {
	key := kr.Key
	if key == "" {
		key = "(bucket)"
	}
	verdict := e.out.Paint(cli.ColorGreen, "allowed")
	if !kr.Allowed {
		verdict = e.out.Paint(cli.ColorRed, "denied")
	}
	by := ""
	switch {
	case kr.Determinant != "":
		by = fmt.Sprintf(" by access rule `%s`", kr.Determinant)
	case kr.Source != "":
		by = " by the " + kr.Source + " policy"
	case !kr.Allowed:
		by = " (no statement allows it)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s%s (%s, %s)", key, verdict, by, principal, action)
	if kr.NormalizedKey != kr.Key {
		fmt.Fprintf(&b, "\n  normalized key: %s", kr.NormalizedKey)
	}
	fmt.Fprintf(&b, "\n  rules: %s", kr.RulesOutcome)
	if len(kr.Matched) > 0 {
		fmt.Fprintf(&b, " (matched: %s)", strings.Join(kr.Matched, ", "))
	}
	if len(kr.BlockedPublic) > 0 {
		fmt.Fprintf(&b, "\n  ignored by the block public access: %s", strings.Join(kr.BlockedPublic, ", "))
	}
	if verbose {
		for _, r := range kr.Rules {
			state := "matches"
			if !r.Matched {
				state = "no " + r.Mismatch
				if r.Condition != "" {
					state += " (" + r.Condition + ")"
				}
			}
			fmt.Fprintf(&b, "\n  %-20s %-5s %6d  %s", r.ID, strings.ToLower(r.Effect), r.Priority, state)
		}
		if kr.Reason != "" {
			fmt.Fprintf(&b, "\n  reason: %s", kr.Reason)
		}
	}
	return b.String()
}

func newAccessPublicSetCmd(e *env) *cobra.Command {
	var (
		pf      patternFlags
		confirm bool
	)
	cmd := &cobra.Command{
		Use:   "set private|public-read|public-read-list|public-prefix|public-write ALIAS/BUCKET",
		Short: "Apply a public access preset to a bucket",
		Long: `Apply a public access preset, implemented as access rules for everyone (their ids
start with "preset-"; the other rules are kept):

  private           no rule for everyone (also removes the allow rules for everyone)
  public-read       everyone reads the whole bucket
  public-read-list  everyone reads and lists
  public-prefix     everyone reads the keys of --prefix, --glob or --regex
  public-write      everyone writes (needs --confirm)`,
		Example: `  nexoctl access public set public-read local/media
  nexoctl access public set public-prefix local/media --glob 'pub/**'`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			preset := args[0]
			switch preset {
			case nexoadmin.PresetPrivate, nexoadmin.PresetPublicRead, nexoadmin.PresetPublicReadList, nexoadmin.PresetPublicPrefix, nexoadmin.PresetPublicWrite:
			default:
				return cli.UsageError("unknown preset %q: private, public-read, public-read-list, public-prefix or public-write", preset)
			}
			match, err := pf.pattern(cmd)
			if err != nil {
				return err
			}
			if preset == nexoadmin.PresetPublicPrefix && match == nil {
				return cli.UsageError("public-prefix needs --prefix, --glob or --regex")
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[1], "access public set")
			if err != nil {
				return err
			}
			if preset == nexoadmin.PresetPublicWrite && !confirm {
				return cli.UsageError("public-write lets everyone write into `%s`: confirm with --confirm", t)
			}
			out, err := client.ApplyAccessPreset(ctx, t.Bucket, nexoadmin.AccessPresetRequest{Preset: preset, Match: match, Confirm: confirm}, "")
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			e.warnRules(out.Warnings)
			state := "not public"
			if out.Public {
				state = "public"
			}
			return e.out.Info(rulesRecordOf(t, "preset", "", out), fmt.Sprintf("Applied the preset `%s` to `%s`: %s.", preset, t, state))
		},
	}
	pf.register(cmd, "keys of public-prefix (and public-write)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm public-write")
	return cmd
}

func newAccessPublicGetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "get ALIAS/BUCKET",
		Short:             "Show whether a bucket is public through its access rules",
		Example:           "  nexoctl access public get local/media",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access public get")
			if err != nil {
				return err
			}
			st, err := client.GetAccessRulesPublic(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			text := fmt.Sprintf("`%s` is not public (%s).", t, st.Preset)
			if st.Public {
				text = fmt.Sprintf("`%s` is public (%s): %s.", t, st.Preset, strings.Join(st.Rules, ", "))
			}
			if st.Blocked {
				text = fmt.Sprintf("`%s` has rules for everyone (%s) ignored by the block public access: %s.", t, st.Preset, strings.Join(st.Rules, ", "))
			}
			return e.out.Result(accessPublicRecord{Alias: t.Alias, AccessRulesPublicStatus: *st}, text)
		},
	}
}

// blockTarget parses ALIAS (the cluster) or ALIAS/BUCKET.
func (e *env) blockTarget(ctx context.Context, arg string) (string, string, cli.AdminClient, error) {
	name, rest, _ := strings.Cut(arg, "/")
	if strings.Trim(rest, "/") == "" {
		alias, client, err := e.adminAlias(ctx, name)
		return alias, "", client, err
	}
	t, client, err := e.bucketAdmin(ctx, arg, "access block-public")
	if err != nil {
		return "", "", nil, err
	}
	return t.Alias, t.Bucket, client, nil
}

// blockScope names the level of a block public access setting.
func blockScope(alias, bucket string) string {
	if bucket == "" {
		return "the cluster `" + alias + "`"
	}
	return "`" + alias + "/" + bucket + "`"
}

func newAccessBlockPublicCmd(e *env, on bool) *cobra.Command {
	var mode string
	use, short := "off ALIAS[/BUCKET]", "Stop blocking the public access"
	if on {
		use, short = "on ALIAS[/BUCKET] [--mode reject|ignore]", "Block the public access"
	}
	cmd := &cobra.Command{
		Use:               use,
		Short:             short,
		Example:           "  nexoctl access block-public on local --mode reject\n  nexoctl access block-public on local/media\n  nexoctl access block-public off local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, bucket, client, err := e.blockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			if mode != "" && bucket != "" {
				return cli.UsageError("--mode is a setting of the cluster (ALIAS)")
			}
			bp, err := client.SetBlockPublic(ctx, bucket, nexoadmin.BlockPublicRequest{Enabled: on, Mode: mode})
			if err != nil {
				return err
			}
			text := fmt.Sprintf("Blocked the public access of %s (mode %s).", blockScope(alias, bucket), bp.Mode)
			if !on {
				text = fmt.Sprintf("Unblocked the public access of %s.", blockScope(alias, bucket))
				if bp.Effective {
					text += " It stays blocked by the cluster setting."
				}
			}
			return e.out.Info(blockPublicRecord{Alias: alias, BlockPublic: *bp}, text)
		},
	}
	if on {
		cmd.Flags().StringVar(&mode, "mode", "", "reject (refuse the rules for everyone) or ignore (accept and ignore them); cluster only")
	}
	return cmd
}

func newAccessBlockPublicStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "status ALIAS[/BUCKET]",
		Short:             "Show the block public access",
		Example:           "  nexoctl access block-public status local/media",
		Args:              exactArgs(1),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, bucket, client, err := e.blockTarget(ctx, args[0])
			if err != nil {
				return err
			}
			bp, err := client.GetBlockPublic(ctx, bucket)
			if err != nil {
				return err
			}
			state := "not blocked"
			if bp.Effective {
				state = "blocked"
			}
			text := fmt.Sprintf("Public access of %s: %s (cluster: %s, mode %s", blockScope(alias, bucket), state, onOff(bp.Cluster), bp.Mode)
			if bucket != "" {
				text += ", bucket: " + onOff(bp.Enabled)
			}
			return e.out.Result(blockPublicRecord{Alias: alias, BlockPublic: *bp}, text+").")
		},
	}
}

// onOff renders a setting.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// accessDocument is the YAML document of the rules of a bucket (access
// export|import).
type accessDocument struct {
	Bucket string                       `json:"bucket,omitempty"`
	Rules  []nexoadmin.BucketAccessRule `json:"rules"`
}

// accessDocumentRecord is the document of access export with --json.
type accessDocumentRecord struct {
	Alias string `json:"alias"`
	accessDocument
}

func newAccessExportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "export ALIAS/BUCKET [FILE]",
		Short: "Export the access rules of a bucket as YAML",
		Long: `Export the access rules of a bucket as a YAML document (bucket, rules) to FILE,
or to the standard output without FILE (JSON with --json): the document of
"nexoctl access import", for the GitOps management of the rules.`,
		Example:           "  nexoctl access export local/media rules.yaml",
		Args:              func(cmd *cobra.Command, args []string) error { return argsBetween(cmd, args, 1, 2) },
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access export")
			if err != nil {
				return err
			}
			cur, err := client.GetAccessRules(ctx, t.Bucket)
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			toStdout := len(args) == 1 || args[1] == "-"
			if toStdout && e.out.JSON() {
				return e.out.Result(accessDocumentRecord{Alias: t.Alias, accessDocument: accessDocument{Bucket: t.Bucket, Rules: cur.Rules}}, "")
			}
			doc, err := json.Marshal(accessDocument{Bucket: t.Bucket, Rules: cur.Rules})
			if err != nil {
				return err
			}
			if toStdout {
				y, err := cli.JSONToYAML(doc)
				if err != nil {
					return err
				}
				_, err = e.out.Stdout().Write(y)
				return err
			}
			y, err := cli.JSONToYAML(doc)
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[1], y, 0o600); err != nil {
				return err
			}
			return e.out.Info(accessExportRecord{Alias: t.Alias, Bucket: t.Bucket, File: args[1], Rules: len(cur.Rules)},
				fmt.Sprintf("Exported %d access %s of `%s` to %s.", len(cur.Rules), plural(len(cur.Rules), "rule", "rules"), t, args[1]))
		},
	}
}

// decodeStrict decodes a JSON document refusing the unknown members.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// readYAMLDocuments reads the documents of a YAML file ("-": stdin) as
// JSON.
func (e *env) readYAMLDocuments(path string) ([][]byte, error) {
	data, err := e.readDocument(path)
	if err != nil {
		return nil, err
	}
	docs, err := cli.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%w: %s holds no document", domain.ErrInvalidArgument, path)
	}
	return docs, nil
}

func newAccessImportCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "import ALIAS/BUCKET FILE",
		Short: "Replace the access rules of a bucket with those of a YAML file",
		Long: `Replace every access rule of the bucket with those of a YAML (or JSON) document:
a mapping with "rules" (the "bucket" member of an export is ignored: the rules
can be imported into another bucket) or a bare list of rules. The server
validates every rule; an invalid one is reported with its position.`,
		Example:           "  nexoctl access import local/media-copy rules.yaml",
		Args:              exactArgs(2),
		ValidArgsFunction: completeTargets(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, err := e.readYAMLDocuments(args[1])
			if err != nil {
				return err
			}
			if len(docs) != 1 {
				return fmt.Errorf("%w: %s holds %d documents, expected one", domain.ErrInvalidArgument, args[1], len(docs))
			}
			var rules []nexoadmin.BucketAccessRule
			if bytes.HasPrefix(bytes.TrimSpace(docs[0]), []byte("[")) {
				err = decodeStrict(docs[0], &rules)
			} else {
				var doc accessDocument
				err = decodeStrict(docs[0], &doc)
				rules = doc.Rules
			}
			if err != nil {
				return fmt.Errorf("%w: %s: %v", domain.ErrInvalidArgument, args[1], err)
			}
			ctx := cmd.Context()
			t, client, err := e.bucketAdmin(ctx, args[0], "access import")
			if err != nil {
				return err
			}
			out, err := editRules(ctx, client, t.Bucket, func([]nexoadmin.BucketAccessRule) ([]nexoadmin.BucketAccessRule, error) { return rules, nil })
			if err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
			e.warnRules(out.Warnings)
			return e.out.Info(rulesRecordOf(t, "imported", "", out),
				fmt.Sprintf("Imported %d access %s into `%s`.", len(out.Rules), plural(len(out.Rules), "rule", "rules"), t))
		},
	}
}
