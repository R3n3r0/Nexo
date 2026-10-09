package s3

import (
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Limits of the form fields of a POST upload (the file excluded).
const (
	maxPostFieldSize = 64 << 10
	maxPostFormSize  = 1 << 20
)

// Form fields with a meaning for the POST upload itself.
const (
	postFieldFile            = "file"
	postFieldPolicy          = "policy"
	postFieldSignature       = "x-amz-signature"
	postFieldCredential      = "x-amz-credential"
	postFieldACL             = "acl"
	postFieldTagging         = "tagging"
	postFieldSuccessStatus   = "success_action_status"
	postFieldSuccessRedirect = "success_action_redirect"
	postFieldRedirect        = "redirect"
)

// PostPolicyDeps are the dependencies of the POST policy routes. Verifier,
// Objects and Policy (which resolves the bucket policies itself) are
// required: without them PostObject answers NotImplemented.
type PostPolicyDeps struct {
	Verifier auth.Verifier
	Objects  object.Service
	Policy   policy.Service
	Enc      httpx.Codec
	Clock    clock.Clock
	Logger   *slog.Logger
}

type postPolicyRoutes struct {
	deps  PostPolicyDeps
	opts  options
	authz authorizer
}

var _ PostPolicyRoutes = (*postPolicyRoutes)(nil)

// NewPostPolicyRoutes returns the POST policy route group
// (WithInsecureSSEC).
func NewPostPolicyRoutes(deps PostPolicyDeps, opts ...Option) PostPolicyRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &postPolicyRoutes{deps: deps, opts: newOptions(opts), authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock, objects: deps.Objects}}
}

// postObjectOperation is the browser upload: POST on the bucket without a
// discriminating query (DeleteObjects uses ?delete).
func postObjectOperation() Operation {
	return Operation{Name: "PostObject", Method: http.MethodPost, Scope: ScopeBucket}
}

// Mount implements Group.
func (h *postPolicyRoutes) Mount(d Dispatcher) {
	available := h.deps.Verifier != nil && h.deps.Objects != nil && h.deps.Policy != nil
	mountAll(d, h.deps.Enc, map[Operation]http.HandlerFunc{postObjectOperation(): h.postObject}, available)
}

// Register implements Routes.
func (h *postPolicyRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *postPolicyRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.deps.Enc.WriteS3Error(w, r, err)
}

func malformedPost(message string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPOSTRequest", message)
}

// postObject reads the form fields (the file must be the last field),
// authenticates the policy, authorizes the signer and streams the file to
// the object layer, which enforces the content-length-range of the policy
// while it reads.
func (h *postPolicyRoutes) postObject(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	form, file, err := readPostForm(r, bucketName)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, postPolicy, err := h.authenticate(r, form)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	r = r.WithContext(middleware.WithIdentity(r.Context(), id))
	// The key the policy conditions were verified against (anonymous
	// uploads use the same substitution).
	key := form.ObjectKey()
	switch {
	case key == "":
		h.fail(w, r, invalidArgument("Bucket POST must contain a field named 'key'."))
		return
	case len(key) > domain.MaxObjectKeyLength:
		h.fail(w, r, domain.NewS3Error(domain.ErrInvalidObjectName, "KeyTooLongError", "Your key is too long."))
		return
	}
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	in, err := h.postInput(r, form, bucketName, key, file)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if lo, hi, ok := postPolicy.LengthRange(); ok {
		in.LengthRange = &object.LengthRange{Min: lo, Max: hi}
	}
	info, err := h.deps.Objects.PutObject(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, r, form, bucketName, key, info)
}

// readPostForm reads the form fields up to the file, which is left
// unread for streaming.
func readPostForm(r *http.Request, bucketName string) (auth.PostForm, *multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return auth.PostForm{}, nil, malformedPost("Bucket POST must be of the enclosure-type multipart/form-data.")
	}
	form := auth.PostForm{Bucket: bucketName, Fields: map[string]string{}}
	total := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return auth.PostForm{}, nil, invalidArgument("POST requires exactly one file upload per request.")
		}
		if err != nil {
			return auth.PostForm{}, nil, malformedPost("The body of your POST request is not well-formed multipart/form-data.")
		}
		name := strings.ToLower(part.FormName())
		if name == postFieldFile {
			form.FileName = baseName(part.FileName())
			return form, part, nil
		}
		value, err := io.ReadAll(io.LimitReader(part, maxPostFieldSize+1))
		if err != nil {
			return auth.PostForm{}, nil, malformedPost("The body of your POST request is not well-formed multipart/form-data.")
		}
		total += len(name) + len(value)
		if len(value) > maxPostFieldSize || total > maxPostFormSize {
			return auth.PostForm{}, nil, domain.NewS3Error(domain.ErrInvalidArgument, "MaxMessageLengthExceeded", "The form fields of your POST request are too large.")
		}
		if name != "" {
			form.Fields[name] = string(value)
		}
	}
}

// baseName strips the directories some browsers send with a file name.
func baseName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// authenticate verifies the signed policy. A form without policy nor
// signature is an anonymous upload (allowed by a bucket policy, if any).
func (h *postPolicyRoutes) authenticate(r *http.Request, form auth.PostForm) (*domain.Identity, *auth.PostPolicy, error) {
	if form.Fields[postFieldPolicy] == "" && form.Fields[postFieldSignature] == "" && form.Fields[postFieldCredential] == "" {
		return identityOf(r), nil, nil
	}
	id, p, err := h.deps.Verifier.VerifyPostPolicy(r.Context(), form)
	if err != nil {
		return nil, nil, err
	}
	if id == nil {
		id = domain.Anonymous()
	}
	return id, p, nil
}

// postInput maps the form fields to the PutObject input: they follow the
// rules of the request headers (metadata, encryption, unsupported
// features).
func (h *postPolicyRoutes) postInput(r *http.Request, form auth.PostForm, bucketName, key string, file io.Reader) (object.PutObjectInput, error) {
	hdr := http.Header{}
	for name, value := range form.Fields {
		if name == postFieldACL {
			name = httpx.HeaderAmzACL
		}
		hdr.Set(name, value)
	}
	if err := checkUnsupportedHeaders(hdr); err != nil {
		return object.PutObjectInput{}, err
	}
	meta, err := metadataFromHeaders(hdr)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	storageClass := hdr.Get(httpx.HeaderAmzStorageClass)
	if err := checkStorageClass(storageClass); err != nil {
		return object.PutObjectInput{}, err
	}
	checksum, err := checksumFromHeaders(hdr)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	fields := r.Clone(r.Context())
	fields.Header = hdr
	sse, err := sseFromHeaders(fields, h.opts.insecureSSEC)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	var tags map[string]string
	if tagging := form.Fields[postFieldTagging]; tagging != "" {
		var doc taggingRequest
		if err := h.deps.Enc.DecodeXML(strings.NewReader(tagging), maxXMLBody, &doc); err != nil {
			return object.PutObjectInput{}, err
		}
		if tags, err = tagsFromXML(doc.TagSet); err != nil {
			return object.PutObjectInput{}, err
		}
	}
	return object.PutObjectInput{
		Bucket: bucketName, Key: key, Reader: file, Size: -1, Checksum: checksum,
		ContentType: meta.contentType, ContentEncoding: meta.contentEncoding, ContentDisposition: meta.contentDisposition,
		ContentLanguage: meta.contentLanguage, CacheControl: meta.cacheControl, Expires: meta.expires,
		UserMetadata: meta.user, Tags: tags, StorageClass: storageClass, SSE: sse,
	}, nil
}

// respond answers a successful upload: a 303 to success_action_redirect
// (with bucket, key and etag in its query), else the status of
// success_action_status (201 carries a PostResponse document), else 204.
func (h *postPolicyRoutes) respond(w http.ResponseWriter, r *http.Request, form auth.PostForm, bucketName, key string, info domain.ObjectInfo) {
	location := objectURL(r, bucketName, key)
	etag := quoteETag(info.ETag)
	w.Header().Set("ETag", etag)
	w.Header().Set("Location", location)
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	writeSSEHeaders(w, info.Encryption)
	if redirect, ok := successRedirect(form.Fields, bucketName, key, etag); ok {
		w.Header().Set("Location", redirect)
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	switch form.Fields[postFieldSuccessStatus] {
	case "200":
		w.WriteHeader(http.StatusOK)
	case "201":
		h.deps.Enc.WriteXML(w, http.StatusCreated, PostResponse{Location: location, Bucket: bucketName, Key: key, ETag: etag})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// successRedirect builds the redirect of a successful upload; a missing or
// invalid (non http/https) URL is ignored, as S3 does.
func successRedirect(fields map[string]string, bucketName, key, etag string) (string, bool) {
	raw := fields[postFieldSuccessRedirect]
	if raw == "" {
		raw = fields[postFieldRedirect]
	}
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	q := u.Query()
	q.Set("bucket", bucketName)
	q.Set("key", key)
	q.Set("etag", etag)
	u.RawQuery = q.Encode()
	return u.String(), true
}
