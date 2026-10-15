package fuzz_test

import (
	"encoding/xml"
	"net/http"
	"testing"
)

// s3Op is an S3 operation that reads a document from the request body.
type s3Op struct {
	name, method, path string
	// seeds are valid and borderline documents.
	seeds []string
}

// s3Ops are the S3 operations with a request document, on the bucket
// "fuzz" (versioned) and on the bucket "fuzz-lock" (object lock), whose
// object "obj" exists; {upload} is a pending multipart upload of fuzz/mp.
var s3Ops = []s3Op{
	{"PutBucketVersioning", http.MethodPut, "/fuzz?versioning", []string{
		`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`,
		`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status><MfaDelete>Disabled</MfaDelete></VersioningConfiguration>`,
	}},
	{"PutBucketTagging", http.MethodPut, "/fuzz?tagging", []string{
		`<Tagging><TagSet><Tag><Key>team</Key><Value>a</Value></Tag></TagSet></Tagging>`,
	}},
	{"PutBucketCors", http.MethodPut, "/fuzz?cors", []string{
		`<CORSConfiguration><CORSRule><AllowedOrigin>https://*.example.com</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedHeader>*</AllowedHeader><MaxAgeSeconds>60</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
	}},
	{"PutBucketLifecycleConfiguration", http.MethodPut, "/fuzz?lifecycle", []string{
		`<LifecycleConfiguration><Rule><ID>r</ID><Filter><And><Prefix>logs/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule></LifecycleConfiguration>`,
	}},
	{"PutBucketNotificationConfiguration", http.MethodPut, "/fuzz?notification", []string{
		`<NotificationConfiguration><QueueConfiguration><Queue>arn:minio:sqs::1:webhook</Queue><Event>s3:ObjectCreated:*</Event><Filter><S3Key><FilterRule><Name>prefix</Name><Value>a/</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`,
		`<NotificationConfiguration/>`,
	}},
	{"PutObjectLockConfiguration", http.MethodPut, "/fuzz-lock?object-lock", []string{
		`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`,
	}},
	{"PutBucketEncryption", http.MethodPut, "/fuzz?encryption", []string{
		`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`,
		`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>nexo-test-key</KMSMasterKeyID></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`,
	}},
	{"PutBucketPolicy", http.MethodPut, "/fuzz?policy", []string{
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::fuzz/public/*"],"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`,
	}},
	{"DeleteObjects", http.MethodPost, "/fuzz?delete", []string{
		`<Delete><Quiet>true</Quiet><Object><Key>missing</Key></Object><Object><Key>obj</Key><VersionId>null</VersionId></Object></Delete>`,
	}},
	{"PutObjectTagging", http.MethodPut, "/fuzz/obj?tagging", []string{
		`<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>`,
	}},
	{"PutObjectRetention", http.MethodPut, "/fuzz-lock/obj?retention", []string{
		`<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>`,
	}},
	{"PutObjectLegalHold", http.MethodPut, "/fuzz-lock/obj?legal-hold", []string{
		`<LegalHold><Status>OFF</Status></LegalHold>`,
	}},
	{"CompleteMultipartUpload", http.MethodPost, "/fuzz/mp?uploadId={upload}", []string{
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"0123456789abcdef0123456789abcdef"</ETag><ChecksumCRC32>AAAAAA==</ChecksumCRC32></Part></CompleteMultipartUpload>`,
	}},
	{"CreateBucket", http.MethodPut, "/fuzz-create", []string{
		`<CreateBucketConfiguration><LocationConstraint>us-east-1</LocationConstraint></CreateBucketConfiguration>`,
	}},
}

// FuzzS3Bodies sends fuzzed documents to the S3 operations that read a
// request body (XML, the bucket policy in JSON).
func FuzzS3Bodies(f *testing.F) {
	tg := start(f)
	base := tg.srv.S3URL
	tg.mustDo(f, http.MethodPut, base+"/fuzz", nil)
	tg.mustDo(f, http.MethodPut, base+"/fuzz?versioning", []byte(s3Ops[0].seeds[0]))
	tg.mustDo(f, http.MethodPut, base+"/fuzz-lock", nil, "X-Amz-Bucket-Object-Lock-Enabled", "true")
	for _, b := range []string{"fuzz", "fuzz-lock"} {
		tg.mustDo(f, http.MethodPut, base+"/"+b+"/obj", []byte("data"))
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(tg.mustDo(f, http.MethodPost, base+"/fuzz/mp?uploads", nil), &created); err != nil || created.UploadID == "" {
		f.Fatalf("create multipart upload: %v", err)
	}
	for i, op := range s3Ops {
		for _, seed := range op.seeds {
			f.Add(uint8(i), []byte(seed))
		}
	}
	f.Fuzz(func(t *testing.T, n uint8, body []byte) {
		if len(body) > maxBody {
			return
		}
		op := s3Ops[int(n)%len(s3Ops)]
		path := op.path
		if op.name == "CompleteMultipartUpload" {
			path = "/fuzz/mp?uploadId=" + created.UploadID
		}
		status, out := tg.do(t, op.method, base+path, body, "Content-Type", "application/xml")
		checkStatus(t, op.name, status, body, out)
	})
}
