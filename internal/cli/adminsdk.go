package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/version"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// adminTimeout bounds one request of the Admin API (the admin commands
// never stream data).
const adminTimeout = 60 * time.Second

// AdminFactoryDeps are the dependencies of the AdminFactory of nexoctl.
type AdminFactoryDeps struct {
	// Clock dates the signatures (nil: the wall clock).
	Clock clock.Clock
	// Transport is the base HTTP transport of the clients (nil: a clone of
	// http.DefaultTransport).
	Transport http.RoundTripper
	// Logger receives the diagnostics of the clients (nil: discarded).
	Logger *slog.Logger
}

type adminFactory struct {
	deps AdminFactoryDeps
}

var _ AdminFactory = (*adminFactory)(nil)

// NewAdminFactory returns the AdminFactory of nexoctl, built on
// pkg/nexoadmin: the endpoint is AdminURL of the alias, the requests are
// signed with SigV4 with the credentials and the region of the alias.
func NewAdminFactory(deps AdminFactoryDeps) AdminFactory {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Transport == nil {
		deps.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	return &adminFactory{deps: deps}
}

// Client implements AdminFactory.
func (f *adminFactory) Client(_ context.Context, a Alias, opts ClientOptions) (AdminClient, error) {
	if err := ValidateAlias(a); err != nil {
		return nil, err
	}
	endpoint, err := AdminURL(a)
	if err != nil {
		return nil, err
	}
	rt := f.deps.Transport
	if opts.Debug != nil {
		rt = newDebugTransport(rt, opts.Debug)
	}
	api, err := nexoadmin.New(nexoadmin.Config{
		Endpoint:   endpoint,
		AccessKey:  a.AccessKey,
		SecretKey:  a.SecretKey,
		Region:     a.Region,
		HTTPClient: &http.Client{Transport: rt, Timeout: adminTimeout},
		UserAgent:  "nexoctl/" + version.Get().Version,
		Now:        f.deps.Clock.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: alias %q: %v", domain.ErrInvalidArgument, a.Name, err)
	}
	f.deps.Logger.Debug("admin client", "alias", a.Name, "endpoint", endpoint)
	return &adminClient{api: api, alias: a.Name, endpoint: endpoint}, nil
}

// AdminURL returns the base URL of the Admin API of an alias (the console
// listener of the server, docs/04 §4.1): Alias.AdminURL when set,
// otherwise the S3 URL of the alias with the next port (9000 → 9001, the
// defaults of nexo server), or the S3 URL itself when it names no port (a
// reverse proxy that routes /api/v1 to the console listener).
func AdminURL(a Alias) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(a.AdminURL), "/")
	if raw != "" {
		if err := checkHTTPURL(raw); err != nil {
			return "", fmt.Errorf("%w: alias %q: admin URL %q must be an http(s) URL", domain.ErrInvalidArgument, a.Name, a.AdminURL)
		}
		return raw, nil
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(a.URL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%w: alias %q: URL %q must be an http(s) URL", domain.ErrInvalidArgument, a.Name, a.URL)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n >= 65535 {
			return "", fmt.Errorf("%w: alias %q: invalid port in %q", domain.ErrInvalidArgument, a.Name, a.URL)
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(n+1))
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// checkHTTPURL checks an absolute http(s) URL.
func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q is not an http(s) URL", domain.ErrInvalidArgument, raw)
	}
	return nil
}

// AdminError is a failure answered by the Admin API (an RFC 9457 problem
// document). It wraps the sentinel of domain matching its code (or its
// HTTP status) and the *nexoadmin.APIError.
type AdminError struct {
	// API is the problem answered by the server.
	API *nexoadmin.APIError
	// Endpoint is the Admin API URL of the alias.
	Endpoint string
	// Alias is the alias the request was sent to (set by the client of
	// an alias): ErrorMessage names it when the command did not.
	Alias string
	// sentinel is the domain error selected by the code or the status.
	sentinel error
}

// Error implements error, in the format of the S3 errors: the class of
// the error (its domain sentinel), the detail of the problem and its
// stable code ("bucket not found: The bucket does not exist.
// (bucket_not_found)"), then one line per located error of a malformed
// policy document.
func (e *AdminError) Error() string {
	if strings.HasPrefix(strings.TrimSpace(e.API.Detail), "<") {
		// An XML (S3) or HTML answer: the URL is not the Admin API.
		return fmt.Sprintf("%s did not answer as the Admin API of a Nexo server (HTTP %d): set the URL of its console listener with \"nexoctl alias set --admin-url\"",
			e.Endpoint, e.API.Status)
	}
	text := e.API.Detail
	if text == "" {
		text = e.API.Title
	}
	if text == "" {
		text = http.StatusText(e.API.Status)
	}
	var b strings.Builder
	if d := e.description(); d != "" && !strings.Contains(strings.ToLower(text), d) {
		b.WriteString(d)
		b.WriteString(": ")
	}
	b.WriteString(text)
	if e.API.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.API.Code)
	}
	for _, d := range e.Details() {
		if d.Message != "" && strings.Contains(text, d.Message) {
			continue // the detail already reports it
		}
		b.WriteString("\n  ")
		b.WriteString(d.String())
	}
	return b.String()
}

// description is the class of the error: the text of its sentinel without
// the sentinels it wraps ("bucket not found", "access denied"), "" without
// a sentinel.
func (e *AdminError) description() string {
	if e.sentinel == nil {
		return ""
	}
	if errors.Is(e.sentinel, ErrAdminNotFound) {
		return domain.ErrNotFound.Error()
	}
	d, _, _ := strings.Cut(e.sentinel.Error(), ": ")
	return strings.ToLower(d)
}

// Unwrap returns the domain sentinel and the problem of the server.
func (e *AdminError) Unwrap() []error {
	if e.sentinel == nil {
		return []error{e.API}
	}
	return []error{e.sentinel, e.API}
}

// Details returns the located errors of a malformed policy document and
// of an invalid access rule.
func (e *AdminError) Details() []ErrorDetail {
	out := make([]ErrorDetail, 0, len(e.API.Errors)+len(e.API.AccessRuleErrors))
	for _, pe := range e.API.Errors {
		out = append(out, ErrorDetail{
			Statement: pe.Statement, Sid: pe.Sid, Element: pe.Element,
			Line: pe.Line, Column: pe.Column, Message: pe.Message,
		})
	}
	for _, ae := range e.API.AccessRuleErrors {
		out = append(out, ErrorDetail{Rule: ae.Rule, RuleID: ae.ID, Field: ae.Field, Position: ae.Position, Message: ae.Message})
	}
	return out
}

// ErrorDetail locates one error of a document: a policy rejected by the
// server (TCL-E-11; Line and Column are 1-based, 0: unknown) or an access
// rule (Rule is 1-based, 0 for the whole list; Position is the 1-based
// position of the problem in the pattern).
type ErrorDetail struct {
	Statement int    `json:"statement"`
	Sid       string `json:"sid,omitempty"`
	Element   string `json:"element,omitempty"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
	Rule      int    `json:"rule,omitempty"`
	RuleID    string `json:"ruleId,omitempty"`
	Field     string `json:"field,omitempty"`
	Position  int    `json:"position,omitempty"`
	Message   string `json:"message"`
}

// String renders "line L, column C: statement N (Sid), Element: message",
// or "rule N (id), field, position P: message" for an access rule.
func (d ErrorDetail) String() string {
	if d.Rule > 0 || d.Field != "" {
		var where []string
		if d.Rule > 0 {
			r := fmt.Sprintf("rule %d", d.Rule)
			if d.RuleID != "" {
				r += fmt.Sprintf(" (%s)", d.RuleID)
			}
			where = append(where, r)
		}
		if d.Field != "" {
			where = append(where, d.Field)
		}
		if d.Position > 0 {
			where = append(where, fmt.Sprintf("position %d", d.Position))
		}
		return strings.Join(where, ", ") + ": " + d.Message
	}
	var parts []string
	if d.Line > 0 {
		parts = append(parts, fmt.Sprintf("line %d, column %d", d.Line, d.Column))
	}
	where := ""
	if d.Statement > 0 {
		where = fmt.Sprintf("statement %d", d.Statement)
		if d.Sid != "" {
			where += fmt.Sprintf(" (%s)", d.Sid)
		}
	}
	if d.Element != "" {
		if where != "" {
			where += ", "
		}
		where += d.Element
	}
	if where != "" {
		parts = append(parts, where)
	}
	if len(parts) == 0 {
		return d.Message
	}
	return strings.Join(parts, ": ") + ": " + d.Message
}

// detailedError is implemented by the errors that carry located details
// (AdminError).
type detailedError interface {
	Details() []ErrorDetail
}

// ErrorDetails returns the located details of err (nil when it has none).
func ErrorDetails(err error) []ErrorDetail {
	var d detailedError
	if errors.As(err, &d) {
		if out := d.Details(); len(out) > 0 {
			return out
		}
	}
	return nil
}

// adminCodeSentinel maps the stable problem codes of the Admin API to the
// sentinels of domain.
func adminCodeSentinel(code string) error {
	switch code {
	case "access_denied", "invalid_csrf_token":
		return domain.ErrAccessDenied
	case "unauthenticated":
		return domain.ErrUnauthenticated
	case "invalid_access_key":
		return domain.ErrInvalidAccessKey
	case "signature_mismatch":
		return domain.ErrSignatureMismatch
	case "request_expired":
		return domain.ErrRequestExpired
	case "user_not_found":
		return domain.ErrUserNotFound
	case "group_not_found":
		return domain.ErrGroupNotFound
	case "policy_not_found":
		return domain.ErrPolicyNotFound
	case "share_not_found":
		return domain.ErrShareNotFound
	case "access_key_not_found":
		return domain.ErrAccessKeyNotFound
	case "bucket_not_found":
		return domain.ErrBucketNotFound
	case "object_not_found":
		return domain.ErrObjectNotFound
	case "version_not_found":
		return domain.ErrNoSuchVersion
	case "upload_not_found":
		return domain.ErrNoSuchUpload
	case "user_already_exists":
		return domain.ErrUserAlreadyExists
	case "bucket_already_exists":
		return domain.ErrBucketAlreadyExists
	case "bucket_not_empty":
		return domain.ErrBucketNotEmpty
	case "malformed_policy":
		return domain.ErrMalformedPolicy
	case "invalid_argument":
		return domain.ErrInvalidArgument
	case "conflict":
		return domain.ErrConflict
	case "not_implemented":
		return domain.ErrNotImplemented
	case "quota_exceeded":
		return domain.ErrQuotaExceeded
	case "entity_too_large":
		return domain.ErrEntityTooLarge
	case nexoadmin.CodeInvalidConfiguration:
		return domain.ErrInvalidArgument
	case nexoadmin.CodeObjectLocked:
		return domain.ErrObjectLocked
	case nexoadmin.CodePreconditionFailed:
		return domain.ErrPreconditionFailed
	case nexoadmin.CodeInvalidAccessRule, nexoadmin.CodeTooManyAccessRules, nexoadmin.CodeConfirmationRequired,
		nexoadmin.CodeInvalidDistribution, nexoadmin.CodeInvalidCertificate:
		return domain.ErrInvalidArgument
	case nexoadmin.CodePublicAccessBlocked:
		return domain.ErrAccessDenied
	case nexoadmin.CodeAliasConflict:
		return domain.ErrConflict
	case nexoadmin.CodeDistributionNotFound, nexoadmin.CodeKeyGroupNotFound, nexoadmin.CodeKeyNotFound, nexoadmin.CodeCertificateNotFound,
		nexoadmin.CodeCachePolicyNotFound, nexoadmin.CodeInvalidationNotFound, nexoadmin.CodeWarmJobNotFound, nexoadmin.CodeWebhookNotFound:
		return ErrAdminNotFound
	case nexoadmin.CodeInvalidCacheRequest:
		return domain.ErrInvalidArgument
	case nexoadmin.CodeBuiltinCachePolicy, nexoadmin.CodeCachePolicyInUse, nexoadmin.CodeAlreadyExists:
		return domain.ErrConflict
	}
	return nil
}

// adminStatusSentinel maps an HTTP status without a known code.
func adminStatusSentinel(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return domain.ErrUnauthenticated
	case http.StatusForbidden:
		return domain.ErrAccessDenied
	case http.StatusNotFound:
		return ErrAdminNotFound
	case http.StatusConflict:
		return domain.ErrConflict
	case http.StatusNotImplemented:
		return domain.ErrNotImplemented
	case http.StatusBadRequest:
		return domain.ErrInvalidArgument
	}
	return nil
}

// ErrAdminNotFound reports a resource of the Admin API that does not exist
// and has no more specific code (exit code 3). It wraps the neutral
// domain.ErrNotFound: a missing group or endpoint is not a missing object.
var ErrAdminNotFound = fmt.Errorf("admin resource not found: %w", domain.ErrNotFound)

// MapAdminError converts an error of pkg/nexoadmin: a problem answered by
// the server becomes an *AdminError wrapping the matching domain sentinel;
// a transport failure names the Admin API endpoint (with a hint about
// --admin-url); an interruption is reported as context.Canceled.
func MapAdminError(err error, endpoint string) error {
	if err == nil {
		return nil
	}
	var api *nexoadmin.APIError
	if errors.As(err, &api) {
		sentinel := adminCodeSentinel(api.Code)
		if sentinel == nil {
			sentinel = adminStatusSentinel(api.Status)
		}
		return &AdminError{API: api, Endpoint: endpoint, sentinel: sentinel}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	// A network failure reads like the one of the S3 API ("cannot reach
	// <url>: connection refused"), with the option that sets the URL.
	if u, ok := unreachable(err); ok {
		if endpoint != "" {
			u.Endpoint = endpoint
		}
		u.Hint = adminURLHint
		return u
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return fmt.Errorf("cannot reach the Admin API at %s: %w (%s)", endpoint, err, adminURLHint)
	}
	return err
}

// adminURLHint tells how to point an alias at the Admin API.
const adminURLHint = `the Admin API: check the console address, or set it with "nexoctl alias set --admin-url"`

// adminClient is the AdminClient of one alias: pkg/nexoadmin with the
// errors mapped by MapAdminError.
type adminClient struct {
	api      nexoadmin.AdminClient
	alias    string
	endpoint string
}

var _ AdminClient = (*adminClient)(nil)

// mapped returns v and err converted by mapError.
func mapped[T any](c *adminClient, v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, c.mapError(err)
	}
	return v, nil
}

// mapError converts err with MapAdminError and records the alias in the
// *AdminError.
func (c *adminClient) mapError(err error) error {
	err = MapAdminError(err, c.endpoint)
	var ae *AdminError
	if errors.As(err, &ae) {
		ae.Alias = c.alias
	}
	return err
}

// ServerInfo implements AdminClient.
func (c *adminClient) ServerInfo(ctx context.Context) (*nexoadmin.ServerInfo, error) {
	v, err := c.api.ServerInfo(ctx)
	return mapped(c, v, err)
}

// DataUsage implements AdminClient.
func (c *adminClient) DataUsage(ctx context.Context) (*nexoadmin.DataUsage, error) {
	v, err := c.api.DataUsage(ctx)
	return mapped(c, v, err)
}

// Heal implements AdminClient.
func (c *adminClient) Heal(ctx context.Context, in nexoadmin.HealRequest) (*nexoadmin.HealJob, error) {
	v, err := c.api.Heal(ctx, in)
	return mapped(c, v, err)
}

// HealStatus implements AdminClient.
func (c *adminClient) HealStatus(ctx context.Context, id string) (*nexoadmin.HealJob, error) {
	v, err := c.api.HealStatus(ctx, id)
	return mapped(c, v, err)
}

// ListHeals implements AdminClient.
func (c *adminClient) ListHeals(ctx context.Context) (*nexoadmin.HealJobList, error) {
	v, err := c.api.ListHeals(ctx)
	return mapped(c, v, err)
}

// RestartService implements AdminClient.
func (c *adminClient) RestartService(ctx context.Context) error {
	return MapAdminError(c.api.RestartService(ctx), c.endpoint)
}

// StopService implements AdminClient.
func (c *adminClient) StopService(ctx context.Context) error {
	return MapAdminError(c.api.StopService(ctx), c.endpoint)
}

// ListUsers implements AdminClient.
func (c *adminClient) ListUsers(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.UserList, error) {
	v, err := c.api.ListUsers(ctx, opts)
	return mapped(c, v, err)
}

// CreateUser implements AdminClient.
func (c *adminClient) CreateUser(ctx context.Context, in nexoadmin.CreateUserRequest) (*nexoadmin.User, error) {
	v, err := c.api.CreateUser(ctx, in)
	return mapped(c, v, err)
}

// GetUser implements AdminClient.
func (c *adminClient) GetUser(ctx context.Context, accessKey string) (*nexoadmin.User, error) {
	v, err := c.api.GetUser(ctx, accessKey)
	return mapped(c, v, err)
}

// UpdateUser implements AdminClient.
func (c *adminClient) UpdateUser(ctx context.Context, accessKey string, in nexoadmin.UpdateUserRequest) (*nexoadmin.User, error) {
	v, err := c.api.UpdateUser(ctx, accessKey, in)
	return mapped(c, v, err)
}

// DeleteUser implements AdminClient.
func (c *adminClient) DeleteUser(ctx context.Context, accessKey string) error {
	return MapAdminError(c.api.DeleteUser(ctx, accessKey), c.endpoint)
}

// ListGroups implements AdminClient.
func (c *adminClient) ListGroups(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.GroupList, error) {
	v, err := c.api.ListGroups(ctx, opts)
	return mapped(c, v, err)
}

// CreateGroup implements AdminClient.
func (c *adminClient) CreateGroup(ctx context.Context, in nexoadmin.CreateGroupRequest) (*nexoadmin.Group, error) {
	v, err := c.api.CreateGroup(ctx, in)
	return mapped(c, v, err)
}

// GetGroup implements AdminClient.
func (c *adminClient) GetGroup(ctx context.Context, name string) (*nexoadmin.Group, error) {
	v, err := c.api.GetGroup(ctx, name)
	return mapped(c, v, err)
}

// UpdateGroup implements AdminClient.
func (c *adminClient) UpdateGroup(ctx context.Context, name string, in nexoadmin.UpdateGroupRequest) (*nexoadmin.Group, error) {
	v, err := c.api.UpdateGroup(ctx, name, in)
	return mapped(c, v, err)
}

// DeleteGroup implements AdminClient.
func (c *adminClient) DeleteGroup(ctx context.Context, name string) error {
	return MapAdminError(c.api.DeleteGroup(ctx, name), c.endpoint)
}

// ListAccessKeys implements AdminClient.
func (c *adminClient) ListAccessKeys(ctx context.Context, opts nexoadmin.ListAccessKeysOptions) (*nexoadmin.AccessKeyList, error) {
	v, err := c.api.ListAccessKeys(ctx, opts)
	return mapped(c, v, err)
}

// CreateAccessKey implements AdminClient.
func (c *adminClient) CreateAccessKey(ctx context.Context, in nexoadmin.CreateAccessKeyRequest) (*nexoadmin.CreatedAccessKey, error) {
	v, err := c.api.CreateAccessKey(ctx, in)
	return mapped(c, v, err)
}

// GetAccessKey implements AdminClient.
func (c *adminClient) GetAccessKey(ctx context.Context, accessKey string) (*nexoadmin.AccessKey, error) {
	v, err := c.api.GetAccessKey(ctx, accessKey)
	return mapped(c, v, err)
}

// UpdateAccessKey implements AdminClient.
func (c *adminClient) UpdateAccessKey(ctx context.Context, accessKey string, in nexoadmin.UpdateAccessKeyRequest) (*nexoadmin.AccessKey, error) {
	v, err := c.api.UpdateAccessKey(ctx, accessKey, in)
	return mapped(c, v, err)
}

// DeleteAccessKey implements AdminClient.
func (c *adminClient) DeleteAccessKey(ctx context.Context, accessKey string) error {
	return MapAdminError(c.api.DeleteAccessKey(ctx, accessKey), c.endpoint)
}

// ListPolicies implements AdminClient.
func (c *adminClient) ListPolicies(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.PolicyList, error) {
	v, err := c.api.ListPolicies(ctx, opts)
	return mapped(c, v, err)
}

// CreatePolicy implements AdminClient.
func (c *adminClient) CreatePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error) {
	v, err := c.api.CreatePolicy(ctx, name, document)
	return mapped(c, v, err)
}

// GetPolicy implements AdminClient.
func (c *adminClient) GetPolicy(ctx context.Context, name string) (*nexoadmin.Policy, error) {
	v, err := c.api.GetPolicy(ctx, name)
	return mapped(c, v, err)
}

// SavePolicy implements AdminClient.
func (c *adminClient) SavePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error) {
	v, err := c.api.SavePolicy(ctx, name, document)
	return mapped(c, v, err)
}

// DeletePolicy implements AdminClient.
func (c *adminClient) DeletePolicy(ctx context.Context, name string) error {
	return MapAdminError(c.api.DeletePolicy(ctx, name), c.endpoint)
}

// AttachPolicy implements AdminClient.
func (c *adminClient) AttachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error {
	return MapAdminError(c.api.AttachPolicy(ctx, in), c.endpoint)
}

// DetachPolicy implements AdminClient.
func (c *adminClient) DetachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error {
	return MapAdminError(c.api.DetachPolicy(ctx, in), c.endpoint)
}

// PolicyEntities implements AdminClient.
func (c *adminClient) PolicyEntities(ctx context.Context, name string) (*nexoadmin.PolicyEntities, error) {
	v, err := c.api.PolicyEntities(ctx, name)
	return mapped(c, v, err)
}

// SimulatePolicy implements AdminClient.
func (c *adminClient) SimulatePolicy(ctx context.Context, in nexoadmin.SimulateRequest) (*nexoadmin.Simulation, error) {
	v, err := c.api.SimulatePolicy(ctx, in)
	return mapped(c, v, err)
}

// ValidatePolicy implements AdminClient.
func (c *adminClient) ValidatePolicy(ctx context.Context, document []byte, bucket string) (*nexoadmin.ValidationReport, error) {
	v, err := c.api.ValidatePolicy(ctx, document, bucket)
	return mapped(c, v, err)
}

// ListConfig implements AdminClient.
func (c *adminClient) ListConfig(ctx context.Context) ([]nexoadmin.ConfigSubsystem, error) {
	v, err := c.api.ListConfig(ctx)
	return mapped(c, v, err)
}

// ConfigSchema implements AdminClient.
func (c *adminClient) ConfigSchema(ctx context.Context) ([]nexoadmin.ConfigSubsystemSchema, error) {
	v, err := c.api.ConfigSchema(ctx)
	return mapped(c, v, err)
}

// GetConfig implements AdminClient.
func (c *adminClient) GetConfig(ctx context.Context, subsystem string) (*nexoadmin.ConfigSubsystem, error) {
	v, err := c.api.GetConfig(ctx, subsystem)
	return mapped(c, v, err)
}

// SetConfig implements AdminClient.
func (c *adminClient) SetConfig(ctx context.Context, subsystem string, settings map[string]string) (*nexoadmin.ConfigSubsystem, error) {
	v, err := c.api.SetConfig(ctx, subsystem, settings)
	return mapped(c, v, err)
}

// DeleteConfig implements AdminClient.
func (c *adminClient) DeleteConfig(ctx context.Context, subsystem string, keys ...string) error {
	return MapAdminError(c.api.DeleteConfig(ctx, subsystem, keys...), c.endpoint)
}

// ExportConfig implements AdminClient.
func (c *adminClient) ExportConfig(ctx context.Context) ([]byte, error) {
	v, err := c.api.ExportConfig(ctx)
	return mapped(c, v, err)
}

// ImportConfig implements AdminClient.
func (c *adminClient) ImportConfig(ctx context.Context, data []byte) error {
	return MapAdminError(c.api.ImportConfig(ctx, data), c.endpoint)
}

// ExportCluster implements AdminClient.
func (c *adminClient) ExportCluster(ctx context.Context) ([]byte, error) {
	v, err := c.api.ExportCluster(ctx)
	return mapped(c, v, err)
}

// ImportCluster implements AdminClient.
func (c *adminClient) ImportCluster(ctx context.Context, archive []byte, opts nexoadmin.ImportClusterOptions) (*nexoadmin.ClusterImportResult, error) {
	v, err := c.api.ImportCluster(ctx, archive, opts)
	return mapped(c, v, err)
}

// ListLocks implements AdminClient.
func (c *adminClient) ListLocks(ctx context.Context) ([]nexoadmin.Lock, error) {
	v, err := c.api.ListLocks(ctx)
	return mapped(c, v, err)
}

// ForceUnlock implements AdminClient.
func (c *adminClient) ForceUnlock(ctx context.Context, resources ...string) error {
	return MapAdminError(c.api.ForceUnlock(ctx, resources...), c.endpoint)
}

// GetBucket implements AdminClient.
func (c *adminClient) GetBucket(ctx context.Context, bucket string) (*nexoadmin.Bucket, error) {
	v, err := c.api.GetBucket(ctx, bucket)
	return mapped(c, v, err)
}

// ApplyBucketLifecycle implements AdminClient.
func (c *adminClient) ApplyBucketLifecycle(ctx context.Context, bucket string) (*nexoadmin.LifecycleApplyResult, error) {
	v, err := c.api.ApplyBucketLifecycle(ctx, bucket)
	return mapped(c, v, err)
}

// ListNotifyTargets implements AdminClient.
func (c *adminClient) ListNotifyTargets(ctx context.Context) ([]nexoadmin.NotifyTarget, error) {
	v, err := c.api.ListNotifyTargets(ctx)
	return mapped(c, v, err)
}

// TestNotifyTarget implements AdminClient.
func (c *adminClient) TestNotifyTarget(ctx context.Context, target string) (*nexoadmin.NotifyTargetTest, error) {
	v, err := c.api.TestNotifyTarget(ctx, target)
	return mapped(c, v, err)
}

// KMSStatus implements AdminClient.
func (c *adminClient) KMSStatus(ctx context.Context) (*nexoadmin.KMSStatus, error) {
	v, err := c.api.KMSStatus(ctx)
	return mapped(c, v, err)
}

// ListKMSKeys implements AdminClient.
func (c *adminClient) ListKMSKeys(ctx context.Context, prefix string) ([]nexoadmin.KMSKey, error) {
	v, err := c.api.ListKMSKeys(ctx, prefix)
	return mapped(c, v, err)
}

// CreateKMSKey implements AdminClient.
func (c *adminClient) CreateKMSKey(ctx context.Context, id string) (*nexoadmin.KMSKey, error) {
	v, err := c.api.CreateKMSKey(ctx, id)
	return mapped(c, v, err)
}

// ResealKMSKeys implements AdminClient.
func (c *adminClient) ResealKMSKeys(ctx context.Context, bucket string) (*nexoadmin.KMSResealResult, error) {
	v, err := c.api.ResealKMSKeys(ctx, bucket)
	return mapped(c, v, err)
}

// GetBucketQuota implements AdminClient.
func (c *adminClient) GetBucketQuota(ctx context.Context, bucket string) (*nexoadmin.BucketQuota, error) {
	v, err := c.api.GetBucketQuota(ctx, bucket)
	return mapped(c, v, err)
}

// SetBucketQuota implements AdminClient.
func (c *adminClient) SetBucketQuota(ctx context.Context, bucket string, quota nexoadmin.BucketQuota) (*nexoadmin.BucketQuota, error) {
	v, err := c.api.SetBucketQuota(ctx, bucket, quota)
	return mapped(c, v, err)
}

// GetBucketEncryption implements AdminClient.
func (c *adminClient) GetBucketEncryption(ctx context.Context, bucket string) (*nexoadmin.BucketEncryption, error) {
	v, err := c.api.GetBucketEncryption(ctx, bucket)
	return mapped(c, v, err)
}

// SetBucketEncryption implements AdminClient.
func (c *adminClient) SetBucketEncryption(ctx context.Context, bucket string, enc nexoadmin.BucketEncryption) (*nexoadmin.BucketEncryption, error) {
	v, err := c.api.SetBucketEncryption(ctx, bucket, enc)
	return mapped(c, v, err)
}

// DeleteBucketEncryption implements AdminClient.
func (c *adminClient) DeleteBucketEncryption(ctx context.Context, bucket string) error {
	return MapAdminError(c.api.DeleteBucketEncryption(ctx, bucket), c.endpoint)
}

// CreateShare implements AdminClient.
func (c *adminClient) CreateShare(ctx context.Context, in nexoadmin.CreateShareRequest) (*nexoadmin.Share, error) {
	v, err := c.api.CreateShare(ctx, in)
	return mapped(c, v, err)
}

// ListShares implements AdminClient.
func (c *adminClient) ListShares(ctx context.Context, opts nexoadmin.ListSharesOptions) (*nexoadmin.ShareList, error) {
	v, err := c.api.ListShares(ctx, opts)
	return mapped(c, v, err)
}

// GetShare implements AdminClient.
func (c *adminClient) GetShare(ctx context.Context, id string) (*nexoadmin.Share, error) {
	v, err := c.api.GetShare(ctx, id)
	return mapped(c, v, err)
}

// RevokeShare implements AdminClient.
func (c *adminClient) RevokeShare(ctx context.Context, id string) error {
	return MapAdminError(c.api.RevokeShare(ctx, id), c.endpoint)
}

// RevokeAllShares implements AdminClient.
func (c *adminClient) RevokeAllShares(ctx context.Context, owner string) (int, error) {
	v, err := c.api.RevokeAllShares(ctx, owner)
	return mapped(c, v, err)
}

// --- access rules, distributions and certificates ---------------------------

// GetAccessRules implements AdminClient.
func (c *adminClient) GetAccessRules(ctx context.Context, bucket string) (*nexoadmin.AccessRules, error) {
	out, err := c.api.GetAccessRules(ctx, bucket)
	return mapped(c, out, err)
}

// SetAccessRules implements AdminClient.
func (c *adminClient) SetAccessRules(ctx context.Context, bucket string, rules []nexoadmin.BucketAccessRule, ifMatch string) (*nexoadmin.AccessRules, error) {
	out, err := c.api.SetAccessRules(ctx, bucket, rules, ifMatch)
	return mapped(c, out, err)
}

// TestAccessRules implements AdminClient.
func (c *adminClient) TestAccessRules(ctx context.Context, bucket string, in nexoadmin.TestAccessRulesRequest) (*nexoadmin.AccessRulesTestResult, error) {
	out, err := c.api.TestAccessRules(ctx, bucket, in)
	return mapped(c, out, err)
}

// GetAccessRulesPublic implements AdminClient.
func (c *adminClient) GetAccessRulesPublic(ctx context.Context, bucket string) (*nexoadmin.AccessRulesPublicStatus, error) {
	out, err := c.api.GetAccessRulesPublic(ctx, bucket)
	return mapped(c, out, err)
}

// ApplyAccessPreset implements AdminClient.
func (c *adminClient) ApplyAccessPreset(ctx context.Context, bucket string, in nexoadmin.AccessPresetRequest, ifMatch string) (*nexoadmin.AccessRules, error) {
	out, err := c.api.ApplyAccessPreset(ctx, bucket, in, ifMatch)
	return mapped(c, out, err)
}

// GetBlockPublic implements AdminClient.
func (c *adminClient) GetBlockPublic(ctx context.Context, bucket string) (*nexoadmin.BlockPublic, error) {
	out, err := c.api.GetBlockPublic(ctx, bucket)
	return mapped(c, out, err)
}

// SetBlockPublic implements AdminClient.
func (c *adminClient) SetBlockPublic(ctx context.Context, bucket string, in nexoadmin.BlockPublicRequest) (*nexoadmin.BlockPublic, error) {
	out, err := c.api.SetBlockPublic(ctx, bucket, in)
	return mapped(c, out, err)
}

// ListDistributions implements AdminClient.
func (c *adminClient) ListDistributions(ctx context.Context) ([]nexoadmin.Distribution, error) {
	out, err := c.api.ListDistributions(ctx)
	return mapped(c, out, err)
}

// CreateDistribution implements AdminClient.
func (c *adminClient) CreateDistribution(ctx context.Context, d nexoadmin.Distribution) (*nexoadmin.Distribution, error) {
	out, err := c.api.CreateDistribution(ctx, d)
	return mapped(c, out, err)
}

// GetDistribution implements AdminClient.
func (c *adminClient) GetDistribution(ctx context.Context, id string) (*nexoadmin.Distribution, error) {
	out, err := c.api.GetDistribution(ctx, id)
	return mapped(c, out, err)
}

// UpdateDistribution implements AdminClient.
func (c *adminClient) UpdateDistribution(ctx context.Context, d nexoadmin.Distribution, ifMatch string) (*nexoadmin.Distribution, error) {
	out, err := c.api.UpdateDistribution(ctx, d, ifMatch)
	return mapped(c, out, err)
}

// TestDistribution implements AdminClient.
func (c *adminClient) TestDistribution(ctx context.Context, id string, in nexoadmin.DistributionTestRequest) (*nexoadmin.DistributionTestResult, error) {
	out, err := c.api.TestDistribution(ctx, id, in)
	return mapped(c, out, err)
}

// RotateOriginSecret implements AdminClient.
func (c *adminClient) RotateOriginSecret(ctx context.Context, id string, in nexoadmin.RotateOriginSecretRequest) (*nexoadmin.RotatedOriginSecret, error) {
	out, err := c.api.RotateOriginSecret(ctx, id, in)
	return mapped(c, out, err)
}

// SignDistributionURL implements AdminClient.
func (c *adminClient) SignDistributionURL(ctx context.Context, id string, in nexoadmin.SignRequest) (*nexoadmin.SignedURL, error) {
	out, err := c.api.SignDistributionURL(ctx, id, in)
	return mapped(c, out, err)
}

// ResolveHost implements AdminClient.
func (c *adminClient) ResolveHost(ctx context.Context, host string) (*nexoadmin.Route, error) {
	out, err := c.api.ResolveHost(ctx, host)
	return mapped(c, out, err)
}

// ListKeyGroups implements AdminClient.
func (c *adminClient) ListKeyGroups(ctx context.Context) ([]nexoadmin.KeyGroup, error) {
	out, err := c.api.ListKeyGroups(ctx)
	return mapped(c, out, err)
}

// CreateKeyGroup implements AdminClient.
func (c *adminClient) CreateKeyGroup(ctx context.Context, in nexoadmin.CreateKeyGroupRequest) (*nexoadmin.CreatedKeyGroup, error) {
	out, err := c.api.CreateKeyGroup(ctx, in)
	return mapped(c, out, err)
}

// GetKeyGroup implements AdminClient.
func (c *adminClient) GetKeyGroup(ctx context.Context, id string) (*nexoadmin.KeyGroup, error) {
	out, err := c.api.GetKeyGroup(ctx, id)
	return mapped(c, out, err)
}

// AddKeyGroupKey implements AdminClient.
func (c *adminClient) AddKeyGroupKey(ctx context.Context, group string, in nexoadmin.AddKeyRequest) (*nexoadmin.AddedKey, error) {
	out, err := c.api.AddKeyGroupKey(ctx, group, in)
	return mapped(c, out, err)
}

// ListCertificates implements AdminClient.
func (c *adminClient) ListCertificates(ctx context.Context) (*nexoadmin.CertificateList, error) {
	out, err := c.api.ListCertificates(ctx)
	return mapped(c, out, err)
}

// UploadCertificate implements AdminClient.
func (c *adminClient) UploadCertificate(ctx context.Context, in nexoadmin.UploadCertificateRequest) (*nexoadmin.Certificate, error) {
	out, err := c.api.UploadCertificate(ctx, in)
	return mapped(c, out, err)
}

// DeleteDistribution implements AdminClient.
func (c *adminClient) DeleteDistribution(ctx context.Context, id string) error {
	return MapAdminError(c.api.DeleteDistribution(ctx, id), c.endpoint)
}

// DeleteKeyGroup implements AdminClient.
func (c *adminClient) DeleteKeyGroup(ctx context.Context, id string) error {
	return MapAdminError(c.api.DeleteKeyGroup(ctx, id), c.endpoint)
}

// DeleteKeyGroupKey implements AdminClient.
func (c *adminClient) DeleteKeyGroupKey(ctx context.Context, group, keyID string) error {
	return MapAdminError(c.api.DeleteKeyGroupKey(ctx, group, keyID), c.endpoint)
}

// DeleteCertificate implements AdminClient.
func (c *adminClient) DeleteCertificate(ctx context.Context, id string) error {
	return MapAdminError(c.api.DeleteCertificate(ctx, id), c.endpoint)
}

// ListDNSProviders implements AdminClient.
func (c *adminClient) ListDNSProviders(ctx context.Context) ([]nexoadmin.DNSProvider, error) {
	out, err := c.api.ListDNSProviders(ctx)
	return mapped(c, out, err)
}

// GetDNSProvider implements AdminClient.
func (c *adminClient) GetDNSProvider(ctx context.Context, name string) (*nexoadmin.DNSProvider, error) {
	out, err := c.api.GetDNSProvider(ctx, name)
	return mapped(c, out, err)
}

// CreateDNSProvider implements AdminClient.
func (c *adminClient) CreateDNSProvider(ctx context.Context, in nexoadmin.DNSProviderRequest) (*nexoadmin.DNSProvider, error) {
	out, err := c.api.CreateDNSProvider(ctx, in)
	return mapped(c, out, err)
}

// UpdateDNSProvider implements AdminClient.
func (c *adminClient) UpdateDNSProvider(ctx context.Context, name string, in nexoadmin.DNSProviderRequest) (*nexoadmin.DNSProvider, error) {
	out, err := c.api.UpdateDNSProvider(ctx, name, in)
	return mapped(c, out, err)
}

// DeleteDNSProvider implements AdminClient.
func (c *adminClient) DeleteDNSProvider(ctx context.Context, name string) error {
	return MapAdminError(c.api.DeleteDNSProvider(ctx, name), c.endpoint)
}

// TestDNSProvider implements AdminClient.
func (c *adminClient) TestDNSProvider(ctx context.Context, name, domain string) (*nexoadmin.DNSProviderTestResult, error) {
	out, err := c.api.TestDNSProvider(ctx, name, domain)
	return mapped(c, out, err)
}

// --- cache of the distributions ----------------------------------------------

// ListCachePolicies implements AdminClient.
func (c *adminClient) ListCachePolicies(ctx context.Context) ([]nexoadmin.CachePolicy, error) {
	out, err := c.api.ListCachePolicies(ctx)
	return mapped(c, out, err)
}

// GetCachePolicy implements AdminClient.
func (c *adminClient) GetCachePolicy(ctx context.Context, name string) (*nexoadmin.CachePolicy, error) {
	out, err := c.api.GetCachePolicy(ctx, name)
	return mapped(c, out, err)
}

// CreateCachePolicy implements AdminClient.
func (c *adminClient) CreateCachePolicy(ctx context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicy, error) {
	out, err := c.api.CreateCachePolicy(ctx, p)
	return mapped(c, out, err)
}

// UpdateCachePolicy implements AdminClient.
func (c *adminClient) UpdateCachePolicy(ctx context.Context, p nexoadmin.CachePolicy, ifMatch string) (*nexoadmin.CachePolicy, error) {
	out, err := c.api.UpdateCachePolicy(ctx, p, ifMatch)
	return mapped(c, out, err)
}

// DeleteCachePolicy implements AdminClient.
func (c *adminClient) DeleteCachePolicy(ctx context.Context, name string) error {
	return MapAdminError(c.api.DeleteCachePolicy(ctx, name), c.endpoint)
}

// PreviewCachePolicy implements AdminClient.
func (c *adminClient) PreviewCachePolicy(ctx context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicyPreview, error) {
	out, err := c.api.PreviewCachePolicy(ctx, p)
	return mapped(c, out, err)
}

// GetCacheSettings implements AdminClient.
func (c *adminClient) GetCacheSettings(ctx context.Context) (*nexoadmin.CacheGlobalSettings, error) {
	out, err := c.api.GetCacheSettings(ctx)
	return mapped(c, out, err)
}

// SetCacheSettings implements AdminClient.
func (c *adminClient) SetCacheSettings(ctx context.Context, s nexoadmin.CacheGlobalSettings, ifMatch string) (*nexoadmin.CacheGlobalSettings, error) {
	out, err := c.api.SetCacheSettings(ctx, s, ifMatch)
	return mapped(c, out, err)
}

// GetDistributionCache implements AdminClient.
func (c *adminClient) GetDistributionCache(ctx context.Context, id string) (*nexoadmin.DistributionCache, error) {
	out, err := c.api.GetDistributionCache(ctx, id)
	return mapped(c, out, err)
}

// SetDistributionCache implements AdminClient.
func (c *adminClient) SetDistributionCache(ctx context.Context, cfg nexoadmin.DistributionCache, ifMatch string) (*nexoadmin.DistributionCache, error) {
	out, err := c.api.SetDistributionCache(ctx, cfg, ifMatch)
	return mapped(c, out, err)
}

// EffectiveCachePolicy implements AdminClient.
func (c *adminClient) EffectiveCachePolicy(ctx context.Context, id, host, path string) (*nexoadmin.CacheEffectivePolicy, error) {
	out, err := c.api.EffectiveCachePolicy(ctx, id, host, path)
	return mapped(c, out, err)
}

// InvalidateCache implements AdminClient.
func (c *adminClient) InvalidateCache(ctx context.Context, distribution string, in nexoadmin.CacheInvalidationRequest) (*nexoadmin.CacheInvalidation, error) {
	out, err := c.api.InvalidateCache(ctx, distribution, in)
	return mapped(c, out, err)
}

// GetCacheInvalidation implements AdminClient.
func (c *adminClient) GetCacheInvalidation(ctx context.Context, distribution, id string) (*nexoadmin.CacheInvalidation, error) {
	out, err := c.api.GetCacheInvalidation(ctx, distribution, id)
	return mapped(c, out, err)
}

// ListCacheInvalidations implements AdminClient.
func (c *adminClient) ListCacheInvalidations(ctx context.Context, distribution string) ([]nexoadmin.CacheInvalidation, error) {
	out, err := c.api.ListCacheInvalidations(ctx, distribution)
	return mapped(c, out, err)
}

// WarmCache implements AdminClient.
func (c *adminClient) WarmCache(ctx context.Context, distribution string, in nexoadmin.CacheWarmRequest) (*nexoadmin.CacheWarmJob, error) {
	out, err := c.api.WarmCache(ctx, distribution, in)
	return mapped(c, out, err)
}

// GetCacheWarmJob implements AdminClient.
func (c *adminClient) GetCacheWarmJob(ctx context.Context, distribution, id string) (*nexoadmin.CacheWarmJob, error) {
	out, err := c.api.GetCacheWarmJob(ctx, distribution, id)
	return mapped(c, out, err)
}

// InspectCache implements AdminClient.
func (c *adminClient) InspectCache(ctx context.Context, in nexoadmin.CacheInspectRequest) (*nexoadmin.CacheInspection, error) {
	out, err := c.api.InspectCache(ctx, in)
	return mapped(c, out, err)
}

// CacheStats implements AdminClient.
func (c *adminClient) CacheStats(ctx context.Context, q nexoadmin.CacheStatsQuery) (*nexoadmin.CacheStats, error) {
	out, err := c.api.CacheStats(ctx, q)
	return mapped(c, out, err)
}

// ListCacheWebhooks implements AdminClient.
func (c *adminClient) ListCacheWebhooks(ctx context.Context, distribution string) ([]nexoadmin.CacheWebhook, error) {
	out, err := c.api.ListCacheWebhooks(ctx, distribution)
	return mapped(c, out, err)
}

// AddCacheWebhook implements AdminClient.
func (c *adminClient) AddCacheWebhook(ctx context.Context, distribution string, w nexoadmin.CacheWebhook) (*nexoadmin.CacheWebhook, error) {
	out, err := c.api.AddCacheWebhook(ctx, distribution, w)
	return mapped(c, out, err)
}

// DeleteCacheWebhook implements AdminClient.
func (c *adminClient) DeleteCacheWebhook(ctx context.Context, distribution, id string) error {
	return MapAdminError(c.api.DeleteCacheWebhook(ctx, distribution, id), c.endpoint)
}

// TestCacheWebhook implements AdminClient.
func (c *adminClient) TestCacheWebhook(ctx context.Context, distribution, id string) (*nexoadmin.CacheWebhookTestResult, error) {
	out, err := c.api.TestCacheWebhook(ctx, distribution, id)
	return mapped(c, out, err)
}
