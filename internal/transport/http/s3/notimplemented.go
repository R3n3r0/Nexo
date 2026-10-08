package s3

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// notImplementedHandler returns a handler answering NotImplemented for op.
func notImplementedHandler(enc httpx.Encoder, op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc.WriteS3Error(w, r, notImplemented(op+" is not implemented"))
	}
}

// mountNotImplemented registers every operation with a NotImplemented handler.
func mountNotImplemented(d Dispatcher, enc httpx.Encoder, ops []Operation) {
	for _, op := range ops {
		d.Handle(op, notImplementedHandler(enc, op.Name))
	}
}

// mountAll registers the handlers of a group in a deterministic order (by
// operation name). When available is false every operation answers
// NotImplemented: the group advertises its routes without its services.
func mountAll(d Dispatcher, enc httpx.Encoder, ops map[Operation]http.HandlerFunc, available bool) {
	names := make([]Operation, 0, len(ops))
	for op := range ops {
		names = append(names, op)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].Name != names[j].Name {
			return names[i].Name < names[j].Name
		}
		return names[i].Method < names[j].Method
	})
	for _, op := range names {
		fn := ops[op]
		if !available {
			fn = notImplementedHandler(enc, op.Name)
		}
		d.Handle(op, fn)
	}
}

// registerStandalone mounts the group on a private dispatcher registered on r.
func registerStandalone(r chi.Router, enc httpx.Encoder, g Group) {
	d := NewDispatcher(DispatcherDeps{Enc: enc})
	g.Mount(d)
	d.Register(r)
}

// unsupportedBucketOperations are the bucket sub-resources Nexo does not
// serve: they answer NotImplemented instead of falling back to the
// default operation of their method (a GET ?website must not list the
// bucket). The configurations Nexo stores but does not enforce yet
// (lifecycle, CORS, encryption, object lock, notifications) refuse their
// PUT for the same reason (M7).
func unsupportedBucketOperations() []Operation {
	var ops []Operation
	for _, sub := range []struct{ query, name string }{
		{"accelerate", "BucketAccelerateConfiguration"},
		{"analytics", "BucketAnalyticsConfiguration"},
		{"intelligent-tiering", "BucketIntelligentTieringConfiguration"},
		{"inventory", "BucketInventoryConfiguration"},
		{"logging", "BucketLogging"},
		{"metrics", "BucketMetricsConfiguration"},
		{"ownershipControls", "BucketOwnershipControls"},
		{"publicAccessBlock", "PublicAccessBlock"},
		{"replication", "BucketReplication"},
		{"requestPayment", "BucketRequestPayment"},
		{"website", "BucketWebsite"},
	} {
		ops = append(ops,
			Operation{Name: "Get" + sub.name, Method: http.MethodGet, Scope: ScopeBucket, Query: sub.query},
			Operation{Name: "Put" + sub.name, Method: http.MethodPut, Scope: ScopeBucket, Query: sub.query},
			Operation{Name: "Delete" + sub.name, Method: http.MethodDelete, Scope: ScopeBucket, Query: sub.query},
		)
	}
	return append(ops,
		Operation{Name: "PutBucketCors", Method: http.MethodPut, Scope: ScopeBucket, Query: "cors"},
		Operation{Name: "PutBucketLifecycle", Method: http.MethodPut, Scope: ScopeBucket, Query: "lifecycle"},
		Operation{Name: "PutBucketEncryption", Method: http.MethodPut, Scope: ScopeBucket, Query: "encryption"},
		Operation{Name: "PutObjectLockConfiguration", Method: http.MethodPut, Scope: ScopeBucket, Query: "object-lock"},
		Operation{Name: "PutBucketNotification", Method: http.MethodPut, Scope: ScopeBucket, Query: "notification"},
	)
}

// unsupportedObjectOperations are the object sub-resources Nexo does not
// serve yet (retention and legal hold come with object lock, M7).
func unsupportedObjectOperations() []Operation {
	return []Operation{
		{Name: "GetObjectRetention", Method: http.MethodGet, Scope: ScopeObject, Query: "retention"},
		{Name: "PutObjectRetention", Method: http.MethodPut, Scope: ScopeObject, Query: "retention"},
		{Name: "GetObjectLegalHold", Method: http.MethodGet, Scope: ScopeObject, Query: "legal-hold"},
		{Name: "PutObjectLegalHold", Method: http.MethodPut, Scope: ScopeObject, Query: "legal-hold"},
		{Name: "GetObjectAttributes", Method: http.MethodGet, Scope: ScopeObject, Query: "attributes"},
		{Name: "GetObjectTorrent", Method: http.MethodGet, Scope: ScopeObject, Query: "torrent"},
		{Name: "RestoreObject", Method: http.MethodPost, Scope: ScopeObject, Query: "restore"},
		{Name: "SelectObjectContent", Method: http.MethodPost, Scope: ScopeObject, Query: "select"},
	}
}
