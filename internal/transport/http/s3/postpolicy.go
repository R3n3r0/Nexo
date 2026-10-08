package s3

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// maxPostFieldSize bounds every non-file form field of a POST upload.
const maxPostFieldSize = 64 << 10

// PostPolicyDeps are the dependencies of the POST policy routes.
type PostPolicyDeps struct {
	Verifier auth.Verifier
	Objects  object.Service
	Buckets  bucket.Service
	Policy   policy.Service
	Enc      httpx.Codec
}

type postPolicyRoutes struct {
	deps  PostPolicyDeps
	authz authorizer
}

var _ PostPolicyRoutes = (*postPolicyRoutes)(nil)

// NewPostPolicyRoutes returns the POST policy route group.
func NewPostPolicyRoutes(deps PostPolicyDeps) PostPolicyRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &postPolicyRoutes{deps: deps, authz: authorizer{policy: deps.Policy, buckets: deps.Buckets, enc: deps.Enc}}
}

// postObjectOperation is the browser upload: POST on the bucket without a
// discriminating query (DeleteObjects uses ?delete).
func postObjectOperation() Operation {
	return Operation{Name: "PostObject", Method: "POST", Scope: ScopeBucket}
}

// Mount implements Group.
func (h *postPolicyRoutes) Mount(d Dispatcher) {
	if h.deps.Objects == nil || h.deps.Verifier == nil {
		d.Handle(postObjectOperation(), notImplemented(h.deps.Enc, "PostObject"))
		return
	}
	d.Handle(postObjectOperation(), h.postObject)
}

// Register implements Routes.
func (h *postPolicyRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

// postObject parses the multipart form (fields first, then the file),
// verifies the policy and stores the object.
func (h *postPolicyRoutes) postObject(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	mr, err := r.MultipartReader()
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data."))
		return
	}
	form := auth.PostForm{Bucket: bucketName, Fields: map[string]string{}}
	var file *multipart.Part
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data."))
			return
		}
		name := strings.ToLower(part.FormName())
		if name == "file" {
			file = part
			form.FileName = part.FileName()
			break // the file must be the last field
		}
		value, err := io.ReadAll(io.LimitReader(part, maxPostFieldSize+1))
		if err != nil || len(value) > maxPostFieldSize {
			h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPOSTRequest", "A form field is too large."))
			return
		}
		form.Fields[name] = string(value)
	}
	if file == nil {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "POST requires a file field."))
		return
	}
	id, postPolicy, err := h.deps.Verifier.VerifyPostPolicy(r.Context(), form)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	r = r.WithContext(middleware.WithIdentity(r.Context(), id))
	key := strings.ReplaceAll(form.Fields["key"], "${filename}", form.FileName)
	if key == "" {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Bucket POST must contain a field named 'key'."))
		return
	}
	if !h.authz.authorize(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}

	// Bound the upload by the policy's content-length-range when present.
	var body io.Reader = file
	_, maxSize, hasRange := postPolicy.LengthRange()
	if hasRange {
		body = io.LimitReader(file, maxSize+1)
	}
	meta := map[string]string{}
	tags := map[string]string{}
	for name, value := range form.Fields {
		switch {
		case strings.HasPrefix(name, "x-amz-meta-"):
			meta[strings.TrimPrefix(name, "x-amz-meta-")] = value
		case name == "cache-control", name == "content-disposition", name == "content-encoding", name == "expires":
			meta[name] = value
		}
	}
	if tagging := form.Fields["tagging"]; tagging != "" {
		var doc taggingRequest
		if err := h.deps.Enc.DecodeXML(strings.NewReader(tagging), maxXMLBody, &doc); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		if tags, err = tagsFromXML(doc.TagSet); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
	}
	info, err := h.deps.Objects.PutObject(r.Context(), object.PutObjectInput{
		Bucket: bucketName, Key: key, Reader: body, Size: -1,
		ContentType: form.Fields["content-type"], UserMetadata: meta, Tags: tags,
		StorageClass: form.Fields["x-amz-storage-class"],
	})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if err := postPolicy.CheckLength(info.Size); err != nil {
		_, _ = h.deps.Objects.DeleteObject(r.Context(), bucketName, key, object.DeleteObjectOptions{VersionID: info.VersionID})
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}

	location := fmt.Sprintf("%s://%s/%s/%s", scheme(r), r.Host, bucketName, key)
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Location", location)
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	if redirect := form.Fields["success_action_redirect"]; redirect != "" {
		http.Redirect(w, r, redirect, http.StatusSeeOther)
		return
	}
	switch form.Fields["success_action_status"] {
	case "200":
		w.WriteHeader(http.StatusOK)
	case "201":
		h.deps.Enc.WriteXML(w, http.StatusCreated, PostResponse{Location: location, Bucket: bucketName, Key: key, ETag: quoteETag(info.ETag)})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
