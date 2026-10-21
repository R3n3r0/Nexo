package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/idp"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// STS protocol constants (AWS Query protocol, API version 2011-06-15).
const (
	// STSVersion is the only API version accepted.
	STSVersion = "2011-06-15"
	// maxSTSBody bounds the form of an STS request (the session policy
	// is the largest parameter).
	maxSTSBody = 64 << 10
	// maxSTSDurationSeconds bounds DurationSeconds before the conversion
	// to a time.Duration (the IAM service enforces the real range, 900 to
	// 43200 seconds).
	maxSTSDurationSeconds = 1 << 32
	// formContentType is the content type of the STS request forms.
	formContentType = "application/x-www-form-urlencoded"
)

// STS actions. AssumeRole is served; the identity-federation actions answer
// NotImplemented until they are (docs/02 S-IAM-08).
const (
	stsActionAssumeRole                 = "AssumeRole"
	stsActionAssumeRoleWithWebIdentity  = "AssumeRoleWithWebIdentity"
	stsActionAssumeRoleWithLDAPIdentity = "AssumeRoleWithLDAPIdentity"
)

// stsUnsupportedActions are the STS actions (AWS and MinIO) Nexo knows but
// does not serve yet.
func stsUnsupportedActions() map[string]bool {
	return map[string]bool{
		"AssumeRoleWithSAML":         true,
		"AssumeRoleWithClientGrants": true,
		"AssumeRoleWithCertificate":  true,
		"AssumeRoleWithCustomToken":  true,
		"AssumeRoot":                 true,
		"GetSessionToken":            true,
		"GetFederationToken":         true,
		"GetCallerIdentity":          true,
		"GetAccessKeyInfo":           true,
		"DecodeAuthorizationMessage": true,
	}
}

// STSDeps are the dependencies of the STS routes. IAM and Policy are
// required: without them every request answers NotImplemented.
type STSDeps struct {
	// IAM issues the temporary credentials.
	IAM iam.Service
	// Policy parses the optional session policy.
	Policy policy.Service
	// IDP authenticates the users of the external identity providers
	// (AssumeRoleWithWebIdentity, AssumeRoleWithLDAPIdentity); nil
	// answers those actions NotImplemented.
	IDP    idp.Service
	Enc    httpx.Codec
	Clock  clock.Clock
	Logger *slog.Logger
}

// STSRoutes exposes the AWS STS API on the S3 port: POST / with a form
// body (Action=AssumeRole&Version=2011-06-15). The caller is the identity
// authenticated by the SigV4 middleware, signed for the service "sts"
// (SigningService); the IAM service decides who may assume a role (root
// and enabled users).
type STSRoutes interface {
	Routes
	Group
}

type stsRoutes struct {
	deps STSDeps
}

var _ STSRoutes = (*stsRoutes)(nil)

// NewSTSRoutes returns the STS route group.
func NewSTSRoutes(deps STSDeps, _ ...Option) STSRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &stsRoutes{deps: deps}
}

// Mount implements Group: STS owns POST on the service scope ("/"), which
// no S3 operation uses.
func (h *stsRoutes) Mount(d Dispatcher) {
	d.Handle(Operation{Name: stsActionAssumeRole, Method: http.MethodPost, Scope: ScopeService}, h.serve)
}

// Register implements Routes.
func (h *stsRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

// AssumeRoleResponse is the body of a successful AssumeRole.
type AssumeRoleResponse struct {
	XMLName          xml.Name            `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleResponse"`
	Result           AssumeRoleResult    `xml:"AssumeRoleResult"`
	ResponseMetadata STSResponseMetadata `xml:"ResponseMetadata"`
}

// AssumeRoleResult carries the temporary credentials.
type AssumeRoleResult struct {
	AssumedRoleUser AssumedRoleUser `xml:"AssumedRoleUser"`
	Credentials     STSCredentials  `xml:"Credentials"`
}

// AssumedRoleUser identifies the session. Nexo has no roles: the ARN names
// the user the session acts as.
type AssumedRoleUser struct {
	Arn           string `xml:"Arn"`
	AssumedRoleID string `xml:"AssumedRoleId"`
}

// STSCredentials are temporary credentials.
type STSCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	// Expiration is an ISO 8601 UTC timestamp.
	Expiration string `xml:"Expiration"`
}

// STSResponseMetadata carries the request id.
type STSResponseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// STSErrorResponse is the error document of the AWS Query protocol.
type STSErrorResponse struct {
	XMLName   xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ ErrorResponse"`
	Error     STSError `xml:"Error"`
	RequestID string   `xml:"RequestId"`
}

// STSError is the error element: Type is "Sender" for client errors and
// "Receiver" for server errors.
type STSError struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// serve decodes the form, selects the action and answers in the STS
// format.
func (h *stsRoutes) serve(w http.ResponseWriter, r *http.Request) {
	if h.deps.IAM == nil || h.deps.Policy == nil {
		h.fail(w, r, fmt.Errorf("%w: STS is not configured", domain.ErrNotImplemented))
		return
	}
	params, err := stsParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	action := params.Get("Action")
	switch {
	case action == "":
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "MissingAction", "Missing Action parameter."))
		return
	case action == stsActionAssumeRole, action == stsActionAssumeRoleWithWebIdentity, action == stsActionAssumeRoleWithLDAPIdentity:
		httpx.SetOperationName(r.Context(), action)
	case stsUnsupportedActions()[action]:
		httpx.SetOperationName(r.Context(), action)
		h.fail(w, r, fmt.Errorf("%w: STS action %s", domain.ErrNotImplemented, action))
		return
	default:
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "InvalidAction", "Could not find operation "+action+" for version "+params.Get("Version")+"."))
		return
	}
	if v := params.Get("Version"); v != STSVersion {
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "Version must be "+STSVersion+"."))
		return
	}
	switch action {
	case stsActionAssumeRoleWithWebIdentity:
		h.assumeRoleWithWebIdentity(w, r, params)
	case stsActionAssumeRoleWithLDAPIdentity:
		h.assumeRoleWithLDAP(w, r, params)
	default:
		h.assumeRole(w, r, params)
	}
}

// assumeRole issues temporary credentials for the caller.
func (h *stsRoutes) assumeRole(w http.ResponseWriter, r *http.Request, params url.Values) {
	ctx := r.Context()
	caller := identityOf(r)
	if caller.IsAnonymous {
		h.fail(w, r, stsError(domain.ErrAccessDenied, "AccessDenied", "Anonymous requests cannot assume a role."))
		return
	}
	in := iam.AssumeRoleInput{Caller: *caller, RoleSessionName: params.Get("RoleSessionName")}
	if raw := params.Get("DurationSeconds"); raw != "" {
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || secs <= 0 || secs > maxSTSDurationSeconds {
			h.fail(w, r, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "DurationSeconds must be a positive integer."))
			return
		}
		in.Duration = time.Duration(secs) * time.Second
	}
	if raw := params.Get("Policy"); raw != "" {
		p, err := h.deps.Policy.Parse(ctx, []byte(raw))
		if err != nil {
			h.fail(w, r, stsError(domain.ErrInvalidArgument, "MalformedPolicyDocument", "The session policy is not valid: "+policyMessage(err)))
			return
		}
		in.Policy = p
	}
	creds, err := h.deps.IAM.AssumeRole(ctx, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	user := caller.EffectiveUser()
	session := in.RoleSessionName
	if session == "" {
		session = creds.AccessKey
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, AssumeRoleResponse{
		Result: AssumeRoleResult{
			AssumedRoleUser: AssumedRoleUser{
				Arn:           "arn:aws:sts:::assumed-role/" + user + "/" + session,
				AssumedRoleID: creds.AccessKey + ":" + session,
			},
			Credentials: STSCredentials{
				AccessKeyID:     creds.AccessKey,
				SecretAccessKey: creds.SecretKey,
				SessionToken:    creds.SessionToken,
				Expiration:      creds.Expiration.UTC().Format(time.RFC3339),
			},
		},
		ResponseMetadata: STSResponseMetadata{RequestID: httpx.RequestIDFrom(ctx)},
	})
}

// sessionParams reads DurationSeconds and Policy, shared by the actions.
func (h *stsRoutes) sessionParams(r *http.Request, params url.Values) (time.Duration, *domain.Policy, error) {
	var d time.Duration
	if raw := params.Get("DurationSeconds"); raw != "" {
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || secs <= 0 || secs > maxSTSDurationSeconds {
			return 0, nil, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "DurationSeconds must be a positive integer.")
		}
		d = time.Duration(secs) * time.Second
	}
	if raw := params.Get("Policy"); raw != "" {
		p, err := h.deps.Policy.Parse(r.Context(), []byte(raw))
		if err != nil {
			return 0, nil, stsError(domain.ErrInvalidArgument, "MalformedPolicyDocument", "The session policy is not valid: "+policyMessage(err))
		}
		return d, p, nil
	}
	return d, nil, nil
}

// federate issues the credentials of a user an identity provider
// authenticated.
func (h *stsRoutes) federate(r *http.Request, params url.Values, id idp.Identity) (*domain.Credentials, string, error) {
	d, p, err := h.sessionParams(r, params)
	if err != nil {
		return nil, "", err
	}
	in := iam.FederatedInput{
		Provider: id.Provider, Subject: id.Subject, Username: id.Username, Groups: id.Groups, PolicyNames: id.PolicyNames,
		Claims: id.Claims, Policy: p, Duration: d, NotAfter: id.Expiry, RoleSessionName: params.Get("RoleSessionName"),
	}
	creds, err := h.deps.IAM.AssumeFederated(r.Context(), in)
	if err != nil {
		return nil, "", err
	}
	session := in.RoleSessionName
	if session == "" {
		session = creds.AccessKey
	}
	return creds, session, nil
}

func stsCredentials(c *domain.Credentials) STSCredentials {
	return STSCredentials{AccessKeyID: c.AccessKey, SecretAccessKey: c.SecretKey, SessionToken: c.SessionToken, Expiration: c.Expiration.UTC().Format(time.RFC3339)}
}

// AssumeRoleWithWebIdentityResponse is the body of a successful
// AssumeRoleWithWebIdentity.
type AssumeRoleWithWebIdentityResponse struct {
	XMLName          xml.Name                        `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleWithWebIdentityResponse"`
	Result           AssumeRoleWithWebIdentityResult `xml:"AssumeRoleWithWebIdentityResult"`
	ResponseMetadata STSResponseMetadata             `xml:"ResponseMetadata"`
}

// AssumeRoleWithWebIdentityResult carries the credentials and the subject
// of the token.
type AssumeRoleWithWebIdentityResult struct {
	SubjectFromWebIdentityToken string          `xml:"SubjectFromWebIdentityToken"`
	Audience                    string          `xml:"Audience,omitempty"`
	AssumedRoleUser             AssumedRoleUser `xml:"AssumedRoleUser"`
	Credentials                 STSCredentials  `xml:"Credentials"`
	Provider                    string          `xml:"Provider,omitempty"`
}

// assumeRoleWithWebIdentity issues credentials for the user of an ID token
// of the OpenID provider. The request needs no signature: the token
// authenticates it.
func (h *stsRoutes) assumeRoleWithWebIdentity(w http.ResponseWriter, r *http.Request, params url.Values) {
	if h.deps.IDP == nil {
		h.fail(w, r, fmt.Errorf("%w: no identity provider", domain.ErrNotImplemented))
		return
	}
	token := params.Get("WebIdentityToken")
	if token == "" {
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "MissingParameter", "WebIdentityToken is required."))
		return
	}
	id, err := h.deps.IDP.WebIdentity(r.Context(), token)
	if err != nil {
		h.fail(w, r, idpError(err, "InvalidIdentityToken"))
		return
	}
	creds, session, err := h.federate(r, params, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	iss, _ := id.Claims["iss"].(string)
	var aud string
	switch a := id.Claims["aud"].(type) {
	case string:
		aud = a
	case []any:
		if len(a) > 0 {
			aud, _ = a[0].(string)
		}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, AssumeRoleWithWebIdentityResponse{
		Result: AssumeRoleWithWebIdentityResult{
			SubjectFromWebIdentityToken: id.Subject, Audience: aud, Provider: iss,
			AssumedRoleUser: AssumedRoleUser{Arn: "arn:aws:sts:::assumed-role/" + iam.FederatedParent(id.Provider, id.Username) + "/" + session, AssumedRoleID: creds.AccessKey + ":" + session},
			Credentials:     stsCredentials(creds),
		},
		ResponseMetadata: STSResponseMetadata{RequestID: httpx.RequestIDFrom(r.Context())},
	})
}

// AssumeRoleWithLDAPIdentityResponse is the body of a successful
// AssumeRoleWithLDAPIdentity (the MinIO extension of STS).
type AssumeRoleWithLDAPIdentityResponse struct {
	XMLName          xml.Name                         `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleWithLDAPIdentityResponse"`
	Result           AssumeRoleWithLDAPIdentityResult `xml:"AssumeRoleWithLDAPIdentityResult"`
	ResponseMetadata STSResponseMetadata              `xml:"ResponseMetadata"`
}

// AssumeRoleWithLDAPIdentityResult carries the credentials.
type AssumeRoleWithLDAPIdentityResult struct {
	Credentials STSCredentials `xml:"Credentials"`
}

// assumeRoleWithLDAP issues credentials for a user of the directory,
// authenticated by its password. The password is only accepted in the
// body: a query string ends up in logs.
func (h *stsRoutes) assumeRoleWithLDAP(w http.ResponseWriter, r *http.Request, params url.Values) {
	if h.deps.IDP == nil {
		h.fail(w, r, fmt.Errorf("%w: no identity provider", domain.ErrNotImplemented))
		return
	}
	if r.URL.Query().Has("LDAPPassword") {
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "LDAPPassword is only accepted in the request body."))
		return
	}
	user, password := params.Get("LDAPUsername"), params.Get("LDAPPassword")
	if user == "" || password == "" {
		h.fail(w, r, stsError(domain.ErrInvalidArgument, "MissingParameter", "LDAPUsername and LDAPPassword are required."))
		return
	}
	id, err := h.deps.IDP.LDAP(r.Context(), user, password)
	if err != nil {
		h.fail(w, r, idpError(err, "AccessDenied"))
		return
	}
	creds, _, err := h.federate(r, params, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, AssumeRoleWithLDAPIdentityResponse{
		Result:           AssumeRoleWithLDAPIdentityResult{Credentials: stsCredentials(creds)},
		ResponseMetadata: STSResponseMetadata{RequestID: httpx.RequestIDFrom(r.Context())},
	})
}

// idpError gives a refusal of the identity provider its STS code.
func idpError(err error, deniedCode string) error {
	if errors.Is(err, domain.ErrAccessDenied) {
		return stsError(domain.ErrAccessDenied, deniedCode, "The identity provider did not accept the credentials.")
	}
	return err
}

// stsParams returns the parameters of an STS request: the query string
// plus the form body (application/x-www-form-urlencoded), the body
// winning.
func stsParams(r *http.Request) (url.Values, error) {
	params := r.URL.Query()
	if r.Body == nil || r.Body == http.NoBody {
		return params, nil
	}
	if ct := r.Header.Get(headerContentType); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != formContentType {
			return nil, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "The request body must be "+formContentType+".")
		}
	}
	body, err := readBody(r, maxSTSBody)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return nil, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "The request is too large.")
		}
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, stsError(domain.ErrInvalidArgument, "InvalidParameterValue", "The request body is not a valid form.")
	}
	for k, vs := range form {
		params[k] = vs
	}
	return params, nil
}

// fail writes err as an STS error document.
func (h *stsRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, message := h.deps.Enc.MapS3Error(err)
	status := code.HTTPStatus
	if status >= http.StatusInternalServerError && status != http.StatusNotImplemented {
		h.deps.Logger.ErrorContext(r.Context(), "sts: request failed", "err", err)
	}
	kind := "Sender"
	if status >= http.StatusInternalServerError {
		kind = "Receiver"
	}
	h.deps.Enc.WriteXML(w, status, STSErrorResponse{
		Error:     STSError{Type: kind, Code: code.Code, Message: message},
		RequestID: httpx.RequestIDFrom(r.Context()),
	})
}

// stsError is an STS error with an explicit code.
func stsError(sentinel error, code, message string) error {
	return domain.NewS3Error(sentinel, code, message)
}

// policyMessage returns the client-facing text of a policy parse error:
// the message of the policy engine's S3 error, a generic text otherwise
// (internal error texts never reach the client).
func policyMessage(err error) string {
	var s3err *domain.S3Error
	if errors.As(err, &s3err) && s3err.Message != "" {
		return s3err.Message
	}
	return "the policy document is malformed."
}
