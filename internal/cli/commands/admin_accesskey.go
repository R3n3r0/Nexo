package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func newAdminAccessKeyCmd(e *env) *cobra.Command {
	cmd := groupCmd("accesskey", "Manage the access keys (service accounts) of the users")
	cmd.Aliases = []string{"access-key", "svcacct"}
	cmd.Long = `Manage access keys: additional credentials of a user (service accounts), with an
optional expiration and an optional inline policy that restricts what the
key can do (the permissions are the intersection with those of the user).`
	cmd.AddCommand(newAdminAccessKeyCreateCmd(e), newAdminAccessKeyListCmd(e), newAdminAccessKeyInfoCmd(e),
		newAdminAccessKeyEditCmd(e), newAdminAccessKeyRemoveCmd(e))
	return cmd
}

func newAdminAccessKeyCreateCmd(e *env) *cobra.Command {
	var user, accessKey, secretKey, name, description, policyFile, expiry string
	cmd := &cobra.Command{
		Use:   "create [flags] ALIAS",
		Short: "Create an access key (the secret key is printed only once)",
		Long: `Create an access key for a user (by default the user of the alias). The secret
key is generated unless --secret-key is given, and is printed only once: store
it immediately. --expiry accepts a duration (30d, 12h) or a date (RFC 3339 or
2006-01-02); --policy restricts the key with an inline policy document.`,
		Example: `  nexoctl admin accesskey create local --expiry 30d --policy photos-ro.json
  nexoctl admin accesskey create local --user alice --name ci --description "CI uploads"`,
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			in := nexoadmin.CreateAccessKeyRequest{ParentUser: user, AccessKey: accessKey, SecretKey: secretKey, Name: name, Description: description}
			if expiry != "" {
				exp, err := e.parseExpiration(expiry)
				if err != nil {
					return err
				}
				in.Expiration = &exp
			}
			if policyFile != "" {
				doc, err := e.readPolicyFile(policyFile)
				if err != nil {
					return err
				}
				in.Policy = doc
			}
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			k, err := client.CreateAccessKey(ctx, in)
			if err != nil {
				return fmt.Errorf("create access key: %w", err)
			}
			rec := accessKeyRecord(alias, &k.AccessKey)
			rec.SecretKey = k.SecretKey
			lines := []string{
				"Access Key: " + k.AccessKey.AccessKey,
				"Secret Key: " + k.SecretKey,
				"Parent User: " + k.ParentUser,
				"Expiration: " + expirationText(k.Expiration),
				"Policy: " + policyText(k.Policy),
				"The secret key is shown only once: store it now.",
			}
			return e.out.Result(rec, strings.Join(lines, "\n"))
		},
	}
	f := cmd.Flags()
	f.StringVar(&user, "user", "", "parent user of the key (default: the user of the alias)")
	f.StringVar(&accessKey, "access-key", "", "access key ID (default: generated)")
	f.StringVar(&secretKey, "secret-key", "", "secret key (default: generated)")
	f.StringVar(&name, "name", "", "name of the key")
	f.StringVar(&description, "description", "", "description of the key")
	f.StringVar(&policyFile, "policy", "", "inline policy document restricting the key (FILE, or - for stdin)")
	f.StringVar(&expiry, "expiry", "", "expiration: a duration (30d) or a date (2026-12-31)")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	return cmd
}

func newAdminAccessKeyListCmd(e *env) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:               "list [flags] ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List access keys (of the user of the alias, or of --user)",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			keys, err := listAccessKeys(ctx, client, user)
			if err != nil {
				return err
			}
			if len(keys) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-24s %-16s %-9s %-23s %s", "ACCESS KEY", "PARENT", "STATUS", "EXPIRATION", "NAME")); err != nil {
					return err
				}
			}
			for i := range keys {
				k := &keys[i]
				text := fmt.Sprintf("%s %-16s %s %-23s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-24s", k.AccessKey)), k.ParentUser,
					statusText(e, k.Status), expirationText(k.Expiration), k.Name)
				if err := e.out.Result(accessKeyRecord(alias, k), strings.TrimRight(text, " ")); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "list the keys of this user")
	_ = cmd.RegisterFlagCompletionFunc("user", completeAdminFlag(e, userNames))
	return cmd
}

func newAdminAccessKeyInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS ACCESS_KEY",
		Short:             "Show an access key (never its secret)",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, accessKeyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			k, err := client.GetAccessKey(ctx, args[1])
			if err != nil {
				return fmt.Errorf("access key %q: %w", args[1], err)
			}
			lines := []string{
				"Access Key: " + k.AccessKey,
				"Parent User: " + k.ParentUser,
				"Status: " + k.Status,
				"Name: " + orDash(k.Name),
				"Description: " + orDash(k.Description),
				"Expiration: " + expirationText(k.Expiration),
				"Created: " + cli.FormatTime(k.CreatedAt),
				"Policy: " + policyText(k.Policy),
			}
			if len(k.Policy) > 0 && string(k.Policy) != "null" {
				lines = append(lines, indentJSON(k.Policy))
			}
			return e.out.Result(accessKeyRecord(alias, k), strings.Join(lines, "\n"))
		},
	}
}

func newAdminAccessKeyEditCmd(e *env) *cobra.Command {
	var (
		secretKey, name, description, policyFile, expiry string
		enable, disable, clearPolicy, clearExpiry        bool
	)
	cmd := &cobra.Command{
		Use:   "edit [flags] ALIAS ACCESS_KEY",
		Short: "Change an access key: secret, status, expiration, inline policy",
		Example: `  nexoctl admin accesskey edit local AKIA0001 --disable
  nexoctl admin accesskey edit local AKIA0001 --expiry 7d --policy narrower.json`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, accessKeyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			var in nexoadmin.UpdateAccessKeyRequest
			changed := false
			if f.Changed("secret-key") {
				in.SecretKey, changed = &secretKey, true
			}
			if f.Changed("name") {
				in.Name, changed = &name, true
			}
			if f.Changed("description") {
				in.Description, changed = &description, true
			}
			switch {
			case enable && disable:
				return cli.UsageError("--enable excludes --disable")
			case enable, disable:
				st := nexoadmin.StatusEnabled
				if disable {
					st = nexoadmin.StatusDisabled
				}
				in.Status, changed = &st, true
			}
			switch {
			case expiry != "" && clearExpiry:
				return cli.UsageError("--expiry excludes --clear-expiry")
			case expiry != "":
				exp, err := e.parseExpiration(expiry)
				if err != nil {
					return err
				}
				in.Expiration, changed = &exp, true
			case clearExpiry:
				in.ClearExpiration, changed = true, true
			}
			switch {
			case policyFile != "" && clearPolicy:
				return cli.UsageError("--policy excludes --clear-policy")
			case policyFile != "":
				doc, err := e.readPolicyFile(policyFile)
				if err != nil {
					return err
				}
				in.Policy, changed = doc, true
			case clearPolicy:
				in.ClearPolicy, changed = true, true
			}
			if !changed {
				return cli.UsageError("nothing to change: give at least one flag")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			k, err := client.UpdateAccessKey(ctx, args[1], in)
			if err != nil {
				return fmt.Errorf("edit access key %q: %w", args[1], err)
			}
			return e.out.Info(accessKeyRecord(alias, k), fmt.Sprintf("Updated access key %q (%s, expiration %s).", k.AccessKey, k.Status, expirationText(k.Expiration)))
		},
	}
	f := cmd.Flags()
	f.StringVar(&secretKey, "secret-key", "", "new secret key")
	f.StringVar(&name, "name", "", "new name")
	f.StringVar(&description, "description", "", "new description")
	f.BoolVar(&enable, "enable", false, "enable the key")
	f.BoolVar(&disable, "disable", false, "disable the key")
	f.StringVar(&expiry, "expiry", "", "new expiration: a duration (30d) or a date (2026-12-31)")
	f.BoolVar(&clearExpiry, "clear-expiry", false, "remove the expiration")
	f.StringVar(&policyFile, "policy", "", "new inline policy document (FILE, or - for stdin)")
	f.BoolVar(&clearPolicy, "clear-policy", false, "remove the inline policy (the key inherits the user's permissions)")
	return cmd
}

func newAdminAccessKeyRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS ACCESS_KEY...",
		Aliases:           []string{"rm"},
		Short:             "Remove access keys (their requests fail immediately)",
		Args:              minArgs(2),
		ValidArgsFunction: completeAdmin(e, accessKeyNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			for _, key := range args[1:] {
				if err := client.DeleteAccessKey(ctx, key); err != nil {
					return fmt.Errorf("remove access key %q: %w", key, err)
				}
				if err := e.out.Info(adminActionRecord{Alias: alias, Type: kindAccessKey, Name: key, Action: "removed"},
					fmt.Sprintf("Removed access key %q.", key)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// parseExpiration parses --expiry: a duration from now (30d, 12h) or a
// date (RFC 3339, or 2006-01-02 at midnight UTC), in the future.
func (e *env) parseExpiration(s string) (time.Time, error) {
	now := e.deps.Clock.Now().UTC()
	var exp time.Time
	if d, err := cli.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, cli.UsageError("--expiry %q must be in the future", s)
		}
		exp = now.Add(d)
	} else if t, err := time.Parse(time.RFC3339, s); err == nil {
		exp = t.UTC()
	} else if t, err := time.Parse(time.DateOnly, s); err == nil {
		exp = t.UTC()
	} else {
		return time.Time{}, cli.UsageError("invalid --expiry %q: use a duration (30d, 12h) or a date (2026-12-31, RFC 3339)", s)
	}
	if !exp.After(now) {
		return time.Time{}, cli.UsageError("--expiry %q must be in the future", s)
	}
	// The wire format has whole seconds: round up, so that the key is valid
	// at least for the requested duration (never a few milliseconds).
	return exp.Add(time.Second - 1).Truncate(time.Second), nil
}

// readPolicyFile reads an inline policy document and checks it is JSON
// (the server validates the policy itself).
func (e *env) readPolicyFile(path string) (json.RawMessage, error) {
	doc, err := e.readDocument(path)
	if err != nil {
		return nil, err
	}
	if err := checkJSON(path, doc); err != nil {
		return nil, err
	}
	return json.RawMessage(doc), nil
}

// checkJSON reports a document that is not JSON with the line and the
// column of the syntax error (a documentError).
func checkJSON(path string, doc []byte) error {
	var v any
	err := json.Unmarshal(doc, &v)
	if err == nil {
		return nil
	}
	detail := cli.ErrorDetail{Message: err.Error()}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		detail.Line, detail.Column = lineColumn(doc, syn.Offset)
	}
	return &documentError{file: path, what: "JSON document", sentinel: domain.ErrInvalidArgument, details: []cli.ErrorDetail{detail}}
}

// lineColumn returns the 1-based line and column of the byte that
// stopped the JSON decoder (json.SyntaxError.Offset counts the bytes read,
// that byte included).
func lineColumn(doc []byte, offset int64) (int, int) {
	line, col := 1, 1
	for i := int64(0); i < offset-1 && i < int64(len(doc)); i++ {
		if doc[i] == '\n' {
			line, col = line+1, 1
			continue
		}
		col++
	}
	return line, col
}

// expirationText renders an expiration ("never" when unset).
func expirationText(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return cli.FormatTime(*t)
}

// policyText summarizes the inline policy of an access key.
func policyText(p json.RawMessage) string {
	if len(p) == 0 || string(p) == "null" {
		return "inherited from the parent user"
	}
	return "inline (restricts the parent user)"
}
