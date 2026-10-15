package fuzz_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

// adminOp is an Admin API operation that reads a JSON document.
type adminOp struct {
	method, path string
	seeds        []string
}

// adminOps are the main Admin API operations with a JSON request body,
// on the bucket "fuzz" (objects "obj" and "ren/a"), the user "fuzzuser", the group
// "fuzzgroup" and the policy "fuzzpol". The operations that start long
// work (heal, speed tests, cache warm-up, configuration import) are left
// out: they have their own limits and tests.
var adminOps = []adminOp{
	{http.MethodPost, "/buckets", []string{`{"name":"fuzz-admin","versioning":true,"objectLock":false,"quotaBytes":1048576}`}},
	{http.MethodPut, "/buckets/fuzz/versioning", []string{`{"status":"Enabled"}`}},
	{http.MethodPut, "/buckets/fuzz/policy", []string{`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::fuzz/*"}]}`}},
	{http.MethodPut, "/buckets/fuzz/quota", []string{`{"type":"hard","size":1073741824}`}},
	{http.MethodPut, "/buckets/fuzz/tags", []string{`{"tags":{"team":"a"}}`}},
	{http.MethodPut, "/buckets/fuzz/objectlock", []string{`{"enabled":true,"mode":"GOVERNANCE","days":1}`}},
	{http.MethodPut, "/buckets/fuzz/encryption", []string{`{"algorithm":"aws:kms","kmsKeyId":"nexo-test-key"}`}},
	{http.MethodPut, "/buckets/fuzz/access", []string{`{"preset":"download","prefix":"pub/"}`}},
	{http.MethodPut, "/buckets/fuzz/lifecycle", []string{`{"rules":[{"id":"r","enabled":true,"filter":{"prefix":"logs/","tags":{"k":"v"},"sizeGreaterThan":10},"expiration":{"days":30},"noncurrentExpiration":{"noncurrentDays":1,"newerNoncurrentVersions":2}},{"id":"mp","enabled":true,"filter":{"prefix":"tmp/"},"abortIncompleteUpload":{"daysAfterInitiation":7}}]}`}},
	{http.MethodPut, "/buckets/fuzz/events", []string{`{"subscriptions":[]}`}},
	{http.MethodPut, "/buckets/fuzz/cors", []string{`{"rules":[{"allowedOrigins":["https://a.example"],"allowedMethods":["GET"],"maxAgeSeconds":60}]}`}},
	{http.MethodDelete, "/buckets/fuzz/objects", []string{`{"objects":[{"key":"missing"}],"prefixes":["tmp/"],"allVersions":false}`}},
	{http.MethodPost, "/buckets/fuzz/objects/folder", []string{`{"prefix":"dir/sub/"}`}},
	{http.MethodPost, "/buckets/fuzz/objects/rename", []string{`{"source":"ren/a","destination":"ren/b","overwrite":false}`}},
	{http.MethodPut, "/buckets/fuzz/objects/tags?key=obj", []string{`{"tags":{"k":"v"}}`}},
	{http.MethodPut, "/buckets/fuzz/objects/retention?key=obj", []string{`{"mode":"GOVERNANCE","retainUntil":"2099-01-01T00:00:00Z"}`}},
	{http.MethodPut, "/buckets/fuzz/objects/legal-hold?key=obj", []string{`{"status":"OFF","versionId":""}`}},
	{http.MethodPost, "/buckets/fuzz/objects/share", []string{`{"key":"obj","permission":"download","expiresIn":3600}`}},
	{http.MethodPost, "/shares", []string{`{"bucket":"fuzz","key":"obj","expiresIn":60}`}},
	{http.MethodPost, "/presign", []string{`{"method":"GET","bucket":"fuzz","key":"obj","expiresIn":60,"headers":{"x-amz-meta-a":"b"}}`}},
	{http.MethodPost, "/post-policy", []string{`{"bucket":"fuzz","keyPrefix":"up/","expiresIn":60,"minSize":1,"maxSize":1024,"successActionStatus":201,"fields":{"x-amz-meta-a":"b"}}`}},
	{http.MethodPost, "/buckets/fuzz/uploads?key=big", []string{`{"contentType":"application/octet-stream","userMetadata":{"a":"b"}}`}},
	{http.MethodPost, "/users", []string{`{"accessKey":"fuzz-new-user","secretKey":"fuzz-secret-123","policyNames":["readonly"],"groups":["fuzzgroup"],"status":"enabled"}`}},
	{http.MethodPut, "/users/fuzzuser", []string{`{"status":"enabled","policyNames":["readwrite"],"groups":["fuzzgroup"]}`}},
	{http.MethodPost, "/groups", []string{`{"name":"fuzz-new-group","members":["fuzzuser"],"policyNames":["readonly"]}`}},
	{http.MethodPut, "/groups/fuzzgroup", []string{`{"status":"enabled","policyNames":["readonly"],"addMembers":["fuzzuser"],"removeMembers":["nobody"]}`}},
	{http.MethodPost, "/access-keys", []string{`{"parentUser":"fuzzuser","name":"k","expiration":"2099-01-01T00:00:00Z","policy":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}}`}},
	{http.MethodPost, "/policies", []string{`{"name":"fuzz-new-pol","document":{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::fuzz/*"],"Condition":{"StringLike":{"s3:prefix":["a*"]}}}]}}`}},
	{http.MethodPut, "/policies/fuzzpol", []string{`{"document":"{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"s3:ListBucket\",\"Resource\":\"arn:aws:s3:::fuzz\"}]}"}`}},
	{http.MethodPost, "/policies/attach", []string{`{"policyNames":["fuzzpol"],"user":"fuzzuser"}`}},
	{http.MethodPost, "/policies/detach", []string{`{"policyNames":["fuzzpol"],"group":"fuzzgroup"}`}},
	{http.MethodPost, "/policies/simulate", []string{`{"user":"fuzzuser","action":"s3:GetObject","bucket":"fuzz","object":"a","sourceIp":"10.1.2.3","secureTransport":true,"time":"2026-01-01T00:00:00Z","conditions":{"s3:prefix":["a"]}}`}},
	{http.MethodPost, "/policies/validate", []string{`{"bucket":"fuzz","document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"*","Condition":{"DateGreaterThan":{"aws:CurrentTime":"2026-01-01T00:00:00Z"}}}]}}`, `{"document":"{}"}`}},
	{http.MethodPost, "/kms/keys", []string{`{"id":"fuzz-key"}`}},
	{http.MethodPut, "/buckets/fuzz/access-rules", []string{`{"rules":[{"id":"r","priority":10,"effect":"allow","principals":["*"],"actions":["s3:GetObject"],"match":{"type":"glob","pattern":"public/**","caseInsensitive":true},"conditions":{"sourceIp":["10.0.0.0/8"],"referer":["https://*.example.com/*"],"methods":["GET"],"headers":{"x-a":"b"}}},{"id":"x","effect":"deny","principals":["user:fuzzuser","group:fuzzgroup"],"actions":["s3:*"],"match":{"type":"regex","pattern":"^(?P<u>[a-z]+)/.*$"}}]}`}},
	{http.MethodPost, "/buckets/fuzz/access-rules/test", []string{`{"keys":["public/a","x"],"action":"read","principal":"*","ip":"10.0.0.1","method":"GET","headers":{"referer":"https://a"}}`}},
	{http.MethodPut, "/buckets/fuzz/access-rules/public", []string{`{"preset":"public-prefix","match":{"type":"prefix","pattern":"pub/"},"confirm":true}`}},
	{http.MethodPut, "/buckets/fuzz/block-public", []string{`{"enabled":true}`}},
	{http.MethodPost, "/distributions", []string{`{"id":"fuzz-dist","enabled":true,"aliases":["cdn.fuzz.test"],"origin":{"bucket":"fuzz","pathPrefix":"site/"},"mode":"website","defaultBehavior":{"access":"public","allowedMethods":["GET","HEAD"],"indexDocument":"index.html","cache":{"cacheControl":"max-age=60"},"cors":{"allowedOrigins":["*"],"maxAge":60}},"behaviors":[{"pathPattern":{"type":"glob","pattern":"/private/**"},"access":"signed","signing":{"keyGroup":"fuzz-kg"}}]}`}},
	{http.MethodPost, "/keygroups", []string{`{"id":"fuzz-kg","comment":"c","generateKey":{"algorithm":"ed25519"}}`}},
	// The seed with a valid key pair is added by FuzzAdminJSON.
	{http.MethodPost, "/certificates", []string{`{"id":"fuzz-cert","certificatePem":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n","privateKeyPem":"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n"}`}},
	{http.MethodPost, "/cache/policies", []string{`{"name":"fuzz-cp","ttl":{"default":"1h","min":0,"max":"1d"},"staleWhileRevalidate":"1m","cacheKey":{"headers":["accept"],"normalize":true},"compression":{"enabled":true,"algorithms":["gzip","br"],"minSize":"1KiB"},"maxObjectSize":"10MiB"}`}},
	{http.MethodPost, "/cache/preview", []string{`{"ttl":{"default":60},"bypass":{"cookies":["session"],"queryParams":["nocache"]},"rangeCaching":{"enabled":true,"blockSize":"1MiB"}}`}},
	{http.MethodPut, "/cache/settings", []string{`{"cache":{"ttl":{"default":"10m"},"respectObjectCacheControl":true},"nodeHeader":true}`}},
	{http.MethodPost, "/cache/invalidations", []string{`{"domain":"cdn.fuzz.test","paths":["/a.css"],"patterns":[{"type":"glob","pattern":"/img/**"}],"tags":["t"],"soft":true}`}},
}

// FuzzAdminJSON sends fuzzed JSON documents to the main operations of
// the Admin API.
func FuzzAdminJSON(f *testing.F) {
	tg := start(f)
	api := tg.srv.ConsoleURL + "/api/v1"
	tg.mustDo(f, http.MethodPut, tg.srv.S3URL+"/fuzz", nil)
	for _, key := range []string{"obj", "ren/a"} {
		tg.mustDo(f, http.MethodPut, tg.srv.S3URL+"/fuzz/"+key, []byte("data"))
	}
	const ct, appJSON = "Content-Type", "application/json"
	tg.mustDo(f, http.MethodPost, api+"/groups", []byte(`{"name":"fuzzgroup"}`), ct, appJSON)
	tg.mustDo(f, http.MethodPost, api+"/users", []byte(`{"accessKey":"fuzzuser","secretKey":"fuzz-secret-123"}`), ct, appJSON)
	tg.mustDo(f, http.MethodPost, api+"/policies", []byte(`{"name":"fuzzpol","document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::fuzz/*"]}]}}`), ct, appJSON)
	for i, op := range adminOps {
		for _, seed := range op.seeds {
			f.Add(uint8(i), []byte(seed))
		}
		if op.path == "/certificates" {
			f.Add(uint8(i), certificateSeed(f))
		}
	}
	f.Fuzz(func(t *testing.T, n uint8, body []byte) {
		if len(body) > maxBody {
			return
		}
		op := adminOps[int(n)%len(adminOps)]
		status, out := tg.do(t, op.method, api+op.path, body, ct, appJSON)
		checkStatus(t, op.method+" "+op.path, status, body, out)
	})
}

// certificateSeed is a certificate upload with a valid self-signed key
// pair of cdn.fuzz.test.
func certificateSeed(f *testing.F) []byte {
	f.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cdn.fuzz.test"},
		DNSNames:     []string{"cdn.fuzz.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		f.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		f.Fatal(err)
	}
	doc, err := json.Marshal(map[string]string{
		"id":             "fuzz-cert-ok",
		"certificatePem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"privateKeyPem":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	})
	if err != nil {
		f.Fatal(err)
	}
	return doc
}
