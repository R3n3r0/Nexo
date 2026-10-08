package integration_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pseudoRandom returns n deterministic pseudo-random bytes (fast, unlike
// crypto/rand, for the large payloads).
func pseudoRandom(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		v := r.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	for i := n - n%8; i < n; i++ {
		b[i] = byte(r.Uint32())
	}
	return b
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// TS-I-01: bucket life cycle: create, list, head, delete (empty and not
// empty).
//
// covers: S-BKT-01, S-BKT-02
func TestBucketLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx

	for _, name := range []string{"alpha", "beta-1", "gamma.2"} {
		e.mkBucket(name)
	}
	list, err := e.root.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	var names []string
	for _, b := range list.Buckets {
		names = append(names, aws.ToString(b.Name))
		assert.False(t, aws.ToTime(b.CreationDate).IsZero())
	}
	assert.Equal(t, []string{"alpha", "beta-1", "gamma.2"}, names, "sorted, system volume hidden")

	_, err = e.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("alpha")})
	require.NoError(t, err)
	_, err = e.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("missing")})
	assert.Equal(t, http.StatusNotFound, httpStatus(err))

	loc, err := e.root.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("alpha")})
	require.NoError(t, err)
	assert.Contains(t, []types.BucketLocationConstraint{"", "us-east-1"}, loc.LocationConstraint)

	// Creating it again conflicts.
	_, err = e.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("alpha")})
	assert.Equal(t, http.StatusConflict, httpStatus(err))
	assert.Equal(t, "BucketAlreadyOwnedByYou", apiCode(err))

	// Not empty: 409 BucketNotEmpty, the bucket stays.
	e.put("beta-1", "obj", []byte("x"))
	_, err = e.root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("beta-1")})
	assert.Equal(t, http.StatusConflict, httpStatus(err))
	assert.Equal(t, "BucketNotEmpty", apiCode(err))
	_, err = e.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("beta-1")})
	require.NoError(t, err)

	// Empty: deleted.
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("beta-1"), Key: aws.String("obj")})
	require.NoError(t, err)
	for _, name := range []string{"alpha", "beta-1"} {
		_, err = e.root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
		require.NoError(t, err, name)
	}
	list, err = e.root.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	require.Len(t, list.Buckets, 1)
	assert.Equal(t, "gamma.2", aws.ToString(list.Buckets[0].Name))
	_, err = e.root.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("alpha")})
	assert.Equal(t, "NoSuchBucket", apiCode(err))

	// Invalid names are refused.
	for _, name := range []string{"ab", "Upper", "under_score", "-dash", "192.168.1.1", strings.Repeat("a", 64)} {
		_, err = e.root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
		assert.Equal(t, "InvalidBucketName", apiCode(err), name)
	}
}

// Regression: "nexo" is a valid S3 bucket name, but /nexo/health/* and
// /nexo/metrics/v1/* are served on the S3 port before the S3 API: objects
// of such a bucket could be written and never read back. The name is
// reserved, as MinIO reserves "minio".
//
// covers: S-BKT-01
func TestReservedBucketName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.root.CreateBucket(e.ctx, &s3.CreateBucketInput{Bucket: aws.String("nexo")})
	require.Error(t, err)
	assert.Equal(t, "InvalidBucketName", apiCode(err))
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))
	resp := e.signedDo(http.MethodPut, "/nexo/health/live", e.rootCreds(), []byte("x"), nil)
	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "no object can be written under /nexo/")
	resp = do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/nexo/health/live", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(resp.Body), `"ok":true`)
}

// TS-I-02: objects of 0 B, 1 KiB, 127 KiB (inline), 129 KiB and 100 MiB
// round-trip with identical content and ETag.
//
// covers: S-OBJ-01
func TestPutGetSizes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("sizes")
	sizes := []struct {
		name string
		size int
	}{
		{"zero", 0},
		{"one-kib", 1 << 10},
		{"inline-127k", 127 << 10},
		{"disk-129k", 129 << 10},
		{"large-100m", 100 << 20},
	}
	for i, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			payload := pseudoRandom(uint64(i+1), tc.size)
			put, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{
				Bucket: aws.String("sizes"), Key: aws.String(tc.name), Body: bytes.NewReader(payload),
			})
			require.NoError(t, err)
			want := md5Hex(payload)
			assert.Equal(t, `"`+want+`"`, aws.ToString(put.ETag))

			head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("sizes"), Key: aws.String(tc.name)})
			require.NoError(t, err)
			assert.Equal(t, int64(tc.size), aws.ToInt64(head.ContentLength))
			assert.Equal(t, `"`+want+`"`, aws.ToString(head.ETag))

			got, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("sizes"), Key: aws.String(tc.name)})
			require.NoError(t, err)
			h := md5.New()
			n, err := io.Copy(h, got.Body)
			require.NoError(t, got.Body.Close())
			require.NoError(t, err)
			assert.Equal(t, int64(tc.size), n)
			assert.Equal(t, want, hex.EncodeToString(h.Sum(nil)), "content round-trips")
			assert.Equal(t, `"`+want+`"`, aws.ToString(got.ETag))
		})
	}
}

// TS-I-03: range reads: bytes=0-0, -500, 100-, beyond the end.
//
// covers: S-OBJ-02
func TestRangeGet(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("ranges")
	// One inline and one on-disk object.
	for _, size := range []int{1000, 200 << 10} {
		key := fmt.Sprintf("obj-%d", size)
		payload := pseudoRandom(uint64(size), size)
		e.put("ranges", key, payload)
		cases := []struct {
			rng           string
			status        int
			from, to      int // inclusive
			contentRange  string
			unsatisfiable bool
		}{
			{rng: "bytes=0-0", status: 206, from: 0, to: 0},
			{rng: "bytes=-500", status: 206, from: size - 500, to: size - 1},
			{rng: "bytes=100-", status: 206, from: 100, to: size - 1},
			{rng: "bytes=10-19", status: 206, from: 10, to: 19},
			{rng: fmt.Sprintf("bytes=%d-%d", size-10, size+500), status: 206, from: size - 10, to: size - 1},
			{rng: fmt.Sprintf("bytes=%d-", size), status: 416, unsatisfiable: true},
			{rng: fmt.Sprintf("bytes=%d-%d", size+10, size+20), status: 416, unsatisfiable: true},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%d/%s", size, tc.rng), func(t *testing.T) {
				req := newRequest(t, http.MethodGet, e.srv.S3URL+"/ranges/"+key, nil)
				req.Header.Set("Range", tc.rng)
				e.signRequest(req, e.rootCreds(), nil)
				resp := do(t, req)
				if tc.unsatisfiable {
					requireS3Error(t, resp, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
					return
				}
				require.Equal(t, tc.status, resp.StatusCode, "%s", resp.Body)
				assert.Equal(t, payload[tc.from:tc.to+1], resp.Body)
				assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", tc.from, tc.to, size), resp.Header.Get("Content-Range"))
				assert.Equal(t, fmt.Sprint(tc.to-tc.from+1), resp.Header.Get("Content-Length"))
				assert.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
			})
		}
	}
	// The SDK sees the same.
	out, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("ranges"), Key: aws.String("obj-1000"), Range: aws.String("bytes=-1")})
	require.NoError(t, err)
	b, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	assert.Len(t, b, 1)
	_, err = e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("ranges"), Key: aws.String("obj-1000"), Range: aws.String("bytes=5000-")})
	assert.Equal(t, "InvalidRange", apiCode(err))
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, httpStatus(err))

	// Regression: no range of an empty object is satisfiable, suffix
	// ranges included (a 206 "bytes */0" is not a valid answer).
	e.put("ranges", "empty", nil)
	for _, rng := range []string{"bytes=0-", "bytes=0-0", "bytes=-1"} {
		req := newRequest(t, http.MethodGet, e.srv.S3URL+"/ranges/empty", nil)
		req.Header.Set("Range", rng)
		e.signRequest(req, e.rootCreds(), nil)
		requireS3Error(t, do(t, req), http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	}
}

// TS-I-04: conditional GET and PUT (If-None-Match: * on PUT): 304/412.
//
// covers: S-OBJ-02
func TestConditionalRequests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("cond")
	etag := e.put("cond", "doc", []byte("version one"))
	quoted := `"` + etag + `"`
	head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("cond"), Key: aws.String("doc")})
	require.NoError(t, err)
	lastMod := aws.ToTime(head.LastModified)
	past, future := lastMod.Add(-time.Hour).UTC().Format(http.TimeFormat), lastMod.Add(time.Hour).UTC().Format(http.TimeFormat)

	get := func(t *testing.T, method string, hdr map[string]string) response {
		req := newRequest(t, method, e.srv.S3URL+"/cond/doc", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		e.signRequest(req, e.rootCreds(), nil)
		return do(t, req)
	}
	reads := []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"if-match ok", map[string]string{"If-Match": quoted}, 200},
		{"if-match star", map[string]string{"If-Match": "*"}, 200},
		{"if-match wrong", map[string]string{"If-Match": `"0123"`}, 412},
		{"if-none-match same", map[string]string{"If-None-Match": quoted}, 304},
		{"if-none-match other", map[string]string{"If-None-Match": `"0123"`}, 200},
		{"if-modified-since future", map[string]string{"If-Modified-Since": future}, 304},
		{"if-modified-since past", map[string]string{"If-Modified-Since": past}, 200},
		{"if-unmodified-since past", map[string]string{"If-Unmodified-Since": past}, 412},
		{"if-unmodified-since future", map[string]string{"If-Unmodified-Since": future}, 200},
		// RFC 9110: If-Match wins over If-Unmodified-Since, If-None-Match
		// over If-Modified-Since.
		{"if-match beats unmodified-since", map[string]string{"If-Match": quoted, "If-Unmodified-Since": past}, 200},
		{"if-none-match beats modified-since", map[string]string{"If-None-Match": `"0123"`, "If-Modified-Since": future}, 200},
	}
	for _, tc := range reads {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				resp := get(t, method, tc.hdr)
				switch tc.status {
				case 200:
					require.Equal(t, 200, resp.StatusCode, "%s", resp.Body)
				case 304:
					requireS3Error(t, resp, http.StatusNotModified, "")
					assert.Equal(t, quoted, resp.Header.Get("ETag"), "304 carries the ETag")
					assert.NotEmpty(t, resp.Header.Get("Last-Modified"))
				case 412:
					requireS3Error(t, resp, http.StatusPreconditionFailed, "PreconditionFailed")
				}
			})
		}
	}

	// PUT If-None-Match: * creates only when absent.
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("cond"), Key: aws.String("doc"),
		Body: strings.NewReader("overwrite"), IfNoneMatch: aws.String("*")})
	assert.Equal(t, http.StatusPreconditionFailed, httpStatus(err))
	assert.Equal(t, "PreconditionFailed", apiCode(err))
	assert.Equal(t, []byte("version one"), e.get("cond", "doc"), "not overwritten")
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("cond"), Key: aws.String("new"),
		Body: strings.NewReader("created"), IfNoneMatch: aws.String("*")})
	require.NoError(t, err)
	// PUT If-Match: only over the expected version.
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("cond"), Key: aws.String("doc"),
		Body: strings.NewReader("v2"), IfMatch: aws.String(`"ffff"`)})
	assert.Equal(t, "PreconditionFailed", apiCode(err))
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("cond"), Key: aws.String("doc"),
		Body: strings.NewReader("v2"), IfMatch: aws.String(quoted)})
	require.NoError(t, err)
	assert.Equal(t, []byte("v2"), e.get("cond", "doc"))
	_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("cond"), Key: aws.String("absent"),
		Body: strings.NewReader("v2"), IfMatch: aws.String(quoted)})
	assert.Error(t, err, "If-Match on a missing object fails")
}

// listV2Raw is the ListObjectsV2 document, decoded independently.
type listV2Raw struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	EncodingType          string   `xml:"EncodingType"`
	Prefix                string   `xml:"Prefix"`
	StartAfter            string   `xml:"StartAfter"`
	KeyCount              int      `xml:"KeyCount"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// TS-I-05: listing of 10k objects with nested prefixes and delimiters,
// pagination by 1 and by 1000, start-after, encoding-type=url: no
// duplicates, nothing missing.
//
// covers: S-OBJ-03
func TestListing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "listing"
	e.mkBucket(bucket)

	// 10 × 10 × 100 = 10000 keys "dNN/sNN/oNNN", uploaded concurrently.
	var want []string
	for d := range 10 {
		for s := range 10 {
			for o := range 100 {
				want = append(want, fmt.Sprintf("d%02d/s%02d/o%03d", d, s, o))
			}
		}
	}
	keys := make(chan string)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 48 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range keys {
				if _, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader(k)}); err != nil {
					errs <- fmt.Errorf("put %s: %w", k, err)
					return
				}
			}
		}()
	}
	for _, k := range want {
		keys <- k
	}
	close(keys)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	t.Run("all by 1000", func(t *testing.T) {
		var got []string
		pages := 0
		p := s3.NewListObjectsV2Paginator(e.root, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1000)})
		for p.HasMorePages() {
			page, err := p.NextPage(e.ctx)
			require.NoError(t, err)
			pages++
			assert.LessOrEqual(t, len(page.Contents), 1000)
			for _, o := range page.Contents {
				got = append(got, aws.ToString(o.Key))
			}
		}
		assert.Equal(t, 10, pages)
		assert.Equal(t, want, got, "every key exactly once, in order")
	})

	t.Run("v1 markers by 1000", func(t *testing.T) {
		var got []string
		marker := ""
		for {
			page, err := e.root.ListObjects(e.ctx, &s3.ListObjectsInput{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1000), Marker: aws.String(marker)})
			require.NoError(t, err)
			for _, o := range page.Contents {
				got = append(got, aws.ToString(o.Key))
			}
			if !aws.ToBool(page.IsTruncated) {
				break
			}
			marker = aws.ToString(page.NextMarker)
			if marker == "" {
				marker = got[len(got)-1]
			}
		}
		assert.Equal(t, want, got)
	})

	t.Run("delimiter levels", func(t *testing.T) {
		top, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Delimiter: aws.String("/")})
		require.NoError(t, err)
		assert.Empty(t, top.Contents)
		require.Len(t, top.CommonPrefixes, 10)
		assert.Equal(t, "d00/", aws.ToString(top.CommonPrefixes[0].Prefix))
		assert.Equal(t, "d09/", aws.ToString(top.CommonPrefixes[9].Prefix))

		mid, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("d03/"), Delimiter: aws.String("/")})
		require.NoError(t, err)
		require.Len(t, mid.CommonPrefixes, 10)
		assert.Equal(t, "d03/s00/", aws.ToString(mid.CommonPrefixes[0].Prefix))

		// Common prefixes paginate too: 10 prefixes, 3 per page.
		var prefixes []string
		p := s3.NewListObjectsV2Paginator(e.root, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("d07/"), Delimiter: aws.String("/"), MaxKeys: aws.Int32(3)})
		for p.HasMorePages() {
			page, err := p.NextPage(e.ctx)
			require.NoError(t, err)
			for _, cp := range page.CommonPrefixes {
				prefixes = append(prefixes, aws.ToString(cp.Prefix))
			}
		}
		require.Len(t, prefixes, 10)
		assert.True(t, sort.StringsAreSorted(prefixes))

		// Delimiter other than "/".
		odd, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("d01/s01/o0"), Delimiter: aws.String("5")})
		require.NoError(t, err)
		var cps []string
		for _, cp := range odd.CommonPrefixes {
			cps = append(cps, aws.ToString(cp.Prefix))
		}
		// The delimiter cuts at the first "5" after the prefix: o050-o059
		// collapse into "o05", o005 into itself.
		assert.Contains(t, cps, "d01/s01/o05")
		assert.Contains(t, cps, "d01/s01/o005")
	})

	t.Run("by 1", func(t *testing.T) {
		var got []string
		p := s3.NewListObjectsV2Paginator(e.root, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("d05/s05/"), MaxKeys: aws.Int32(1)})
		for p.HasMorePages() {
			page, err := p.NextPage(e.ctx)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page.Contents), 1)
			for _, o := range page.Contents {
				got = append(got, aws.ToString(o.Key))
			}
		}
		require.Len(t, got, 100)
		assert.Equal(t, want[5*1000+5*100:5*1000+6*100], got)
	})

	t.Run("start-after", func(t *testing.T) {
		out, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), StartAfter: aws.String("d09/s09/o095"), MaxKeys: aws.Int32(10)})
		require.NoError(t, err)
		var got []string
		for _, o := range out.Contents {
			got = append(got, aws.ToString(o.Key))
		}
		assert.Equal(t, []string{"d09/s09/o096", "d09/s09/o097", "d09/s09/o098", "d09/s09/o099"}, got)
		assert.False(t, aws.ToBool(out.IsTruncated))
		// StartAfter between keys.
		out, err = e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), StartAfter: aws.String("d04/s09/o099z"), MaxKeys: aws.Int32(1)})
		require.NoError(t, err)
		require.Len(t, out.Contents, 1)
		assert.Equal(t, "d05/s00/o000", aws.ToString(out.Contents[0].Key))
	})

	t.Run("encoding-type url", func(t *testing.T) {
		special := []string{"enc/a b.txt", "enc/caffè.txt", "enc/q?x=1&y=2", "enc/per%cent", "enc/plus+sign", "enc/tab\there"}
		for _, k := range special {
			e.put(bucket, k, []byte(k))
		}
		q := url.Values{"list-type": {"2"}, "prefix": {"enc/"}, "encoding-type": {"url"}}
		resp := e.signedDo(http.MethodGet, "/"+bucket+"?"+q.Encode(), e.rootCreds(), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		var doc listV2Raw
		require.NoError(t, xml.Unmarshal(resp.Body, &doc))
		assert.Equal(t, "url", doc.EncodingType)
		var got []string
		for _, c := range doc.Contents {
			assert.NotContains(t, c.Key, " ", "keys are URL-encoded: %q", c.Key)
			dec, err := url.QueryUnescape(c.Key)
			require.NoError(t, err, c.Key)
			got = append(got, dec)
		}
		sorted := append([]string(nil), special...)
		sort.Strings(sorted)
		assert.Equal(t, sorted, got)
		// Without encoding the keys are sent verbatim (XML-escaped).
		resp = e.signedDo(http.MethodGet, "/"+bucket+"?list-type=2&prefix=enc%2F", e.rootCreds(), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		doc = listV2Raw{}
		require.NoError(t, xml.Unmarshal(resp.Body, &doc))
		got = nil
		for _, c := range doc.Contents {
			got = append(got, c.Key)
		}
		assert.Equal(t, sorted, got)
	})
}

// TS-I-05 (edge cases): max-keys=0 returns no key and is not truncated;
// a start-after inside a common prefix keeps the prefix when keys after
// it roll up into it, as AWS does.
//
// covers: S-OBJ-03
func TestListingEdgeCases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "listedge"
	e.mkBucket(bucket)
	for _, k := range []string{"a/b", "a/c", "b"} {
		e.put(bucket, k, []byte(k))
	}
	list := func(query string) listV2Raw {
		t.Helper()
		resp := e.signedDo(http.MethodGet, "/"+bucket+"?"+query, e.rootCreds(), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		var doc listV2Raw
		require.NoError(t, xml.Unmarshal(resp.Body, &doc))
		assert.Contains(t, string(resp.Body), "<MaxKeys>")
		return doc
	}
	doc := list("list-type=2&max-keys=0")
	assert.Zero(t, doc.KeyCount)
	assert.Empty(t, doc.Contents)
	assert.False(t, doc.IsTruncated)
	resp := e.signedDo(http.MethodGet, "/"+bucket+"?list-type=2&max-keys=0", e.rootCreds(), nil, nil)
	assert.Contains(t, string(resp.Body), "<MaxKeys>0</MaxKeys>")
	requireS3Error(t, e.signedDo(http.MethodGet, "/missing-bucket?list-type=2&max-keys=0", e.rootCreds(), nil, nil), http.StatusNotFound, "NoSuchBucket")

	doc = list("list-type=2&delimiter=%2F&start-after=a%2Fb")
	require.Len(t, doc.CommonPrefixes, 1)
	assert.Equal(t, "a/", doc.CommonPrefixes[0].Prefix)
	require.Len(t, doc.Contents, 1)
	assert.Equal(t, "b", doc.Contents[0].Key)
	doc = list("list-type=2&delimiter=%2F&start-after=a%2Fc")
	assert.Empty(t, doc.CommonPrefixes, "no key of a/ follows a/c")
}

// TS-I-12: versioning: three puts, delete (marker), get of a specific
// version, delete of a version, suspend.
//
// covers: S-BKT-04, S-OBJ-10
func TestVersioning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "versions"
	e.mkBucket(bucket)
	ctx := e.ctx
	_, err := e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	vs, err := e.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Equal(t, types.BucketVersioningStatusEnabled, vs.Status)

	var ids []string
	for i := 1; i <= 3; i++ {
		out, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), Body: strings.NewReader(fmt.Sprintf("v%d", i))})
		require.NoError(t, err)
		require.NotEmpty(t, aws.ToString(out.VersionId))
		ids = append(ids, aws.ToString(out.VersionId))
	}
	assert.Len(t, uniq(ids), 3)
	assert.Equal(t, []byte("v3"), e.get(bucket, "doc"))

	getVersion := func(id string) (string, error) {
		out, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(id)})
		if err != nil {
			return "", err
		}
		defer func() { _ = out.Body.Close() }()
		b, err := io.ReadAll(out.Body)
		assert.Equal(t, id, aws.ToString(out.VersionId))
		return string(b), err
	}
	for i, id := range ids {
		got, err := getVersion(id)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("v%d", i+1), got)
	}

	// Delete without version: a delete marker hides the object.
	del, err := e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(del.DeleteMarker))
	marker := aws.ToString(del.VersionId)
	require.NotEmpty(t, marker)
	_, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")})
	assert.Equal(t, "NoSuchKey", apiCode(err))
	resp := e.signedDo(http.MethodGet, "/"+bucket+"/doc", e.rootCreds(), nil, nil)
	requireS3Error(t, resp, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "true", resp.Header.Get("x-amz-delete-marker"))
	got, err := getVersion(ids[1])
	require.NoError(t, err, "older versions stay readable")
	assert.Equal(t, "v2", got)
	list, err := e.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Empty(t, list.Contents, "the marker hides the key from listings")

	versions, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, versions.Versions, 3)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.Equal(t, marker, aws.ToString(versions.DeleteMarkers[0].VersionId))
	assert.True(t, aws.ToBool(versions.DeleteMarkers[0].IsLatest))
	assert.Equal(t, ids[2], aws.ToString(versions.Versions[0].VersionId), "newest first")

	// Deleting a specific version removes it for good.
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(ids[0])})
	require.NoError(t, err)
	_, err = getVersion(ids[0])
	assert.Contains(t, []string{"NoSuchVersion", "NoSuchKey"}, apiCode(err))
	// Removing the marker brings v3 back.
	del, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String(marker)})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(del.DeleteMarker))
	assert.Equal(t, []byte("v3"), e.get(bucket, "doc"))

	// Suspend: new writes replace the null version.
	_, err = e.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	require.NoError(t, err)
	vs, err = e.root.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Equal(t, types.BucketVersioningStatusSuspended, vs.Status)
	for i := 4; i <= 5; i++ {
		out, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), Body: strings.NewReader(fmt.Sprintf("v%d", i))})
		require.NoError(t, err)
		assert.Contains(t, []string{"", "null"}, aws.ToString(out.VersionId))
	}
	versions, err = e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	var listed []string
	for _, v := range versions.Versions {
		listed = append(listed, aws.ToString(v.VersionId))
	}
	assert.Equal(t, []string{"null", ids[2], ids[1]}, listed, "one null version replaced in place")
	got, err = getVersion("null")
	require.NoError(t, err)
	assert.Equal(t, "v5", got)

	// Version listings paginate one entry at a time across null, regular
	// versions and delete markers of several keys.
	for _, k := range []string{"a", "z"} {
		e.put(bucket, k, []byte(k))
		_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(k)})
		require.NoError(t, err)
	}
	all, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	type entry struct{ key, id string }
	var whole []entry
	for _, v := range all.Versions {
		whole = append(whole, entry{aws.ToString(v.Key), aws.ToString(v.VersionId)})
	}
	for _, m := range all.DeleteMarkers {
		whole = append(whole, entry{aws.ToString(m.Key), aws.ToString(m.VersionId)})
	}
	var paged []entry
	var keyMarker, idMarker *string
	for range 50 {
		page, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1),
			KeyMarker: keyMarker, VersionIdMarker: idMarker})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Versions)+len(page.DeleteMarkers), 1)
		for _, v := range page.Versions {
			paged = append(paged, entry{aws.ToString(v.Key), aws.ToString(v.VersionId)})
		}
		for _, m := range page.DeleteMarkers {
			paged = append(paged, entry{aws.ToString(m.Key), aws.ToString(m.VersionId)})
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		keyMarker, idMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	assert.ElementsMatch(t, whole, paged, "paging by one returns every version once")
	// doc: the null version and two older ones; a and z: in a suspended
	// bucket the null delete marker replaces the null version.
	assert.Len(t, paged, 5)
	var markers []string
	for _, m := range all.DeleteMarkers {
		markers = append(markers, aws.ToString(m.Key)+"@"+aws.ToString(m.VersionId))
	}
	assert.Equal(t, []string{"a@null", "z@null"}, markers)
	assert.Equal(t, []byte("v5"), e.get(bucket, "doc"))
	// Versioning cannot go back to unversioned.
	_, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"), VersionId: aws.String("not-a-version")})
	assert.Error(t, err)
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// TS-I-13: 100 goroutines PUT the same key while readers GET it: every
// read returns one complete, consistent object.
//
// covers: S-OBJ-01, S-STO-01
func TestConcurrentPutSameKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket, key = "race", "hot"
	e.mkBucket(bucket)
	const writers, size = 100, 160 << 10 // beyond the inline limit
	payloads := make([][]byte, writers)
	digests := map[string]int{}
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte(i)}, size)
		copy(payloads[i], fmt.Sprintf("writer-%03d|", i))
		digests[md5Hex(payloads[i])] = i
	}
	e.put(bucket, key, payloads[0])

	stop := make(chan struct{})
	var readers sync.WaitGroup
	var reads, readErrs sync.Map
	for r := range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			n := 0
			for {
				select {
				case <-stop:
					reads.Store(r, n)
					return
				default:
				}
				out, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
				if err != nil {
					readErrs.Store(fmt.Sprintf("%d-%d", r, n), err)
					continue
				}
				b, err := io.ReadAll(out.Body)
				_ = out.Body.Close()
				if err != nil {
					readErrs.Store(fmt.Sprintf("%d-%d", r, n), err)
					continue
				}
				i, ok := digests[md5Hex(b)]
				if !ok || len(b) != size {
					readErrs.Store(fmt.Sprintf("%d-%d", r, n), fmt.Errorf("torn read: %d bytes", len(b)))
					continue
				}
				if etag := strings.Trim(aws.ToString(out.ETag), `"`); etag != md5Hex(payloads[i]) {
					readErrs.Store(fmt.Sprintf("%d-%d", r, n), fmt.Errorf("etag %s does not match the content of writer %d", etag, i))
				}
				n++
			}
		}()
	}

	var wg sync.WaitGroup
	putErrs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payloads[i])}); err != nil {
				putErrs <- fmt.Errorf("writer %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(putErrs)
	for err := range putErrs {
		assert.NoError(t, err)
	}
	readErrs.Range(func(k, v any) bool {
		t.Errorf("read %v: %v", k, v)
		return true
	})
	total := 0
	reads.Range(func(_, v any) bool { total += v.(int); return true })
	assert.Positive(t, total, "readers made progress")

	final := e.get(bucket, key)
	_, ok := digests[md5Hex(final)]
	assert.True(t, ok, "the final object is one of the writes")
	list, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	assert.Equal(t, int64(size), aws.ToInt64(list.Contents[0].Size))
	assertNoTmp(t, e)
}

// TS-I-14: read-after-write: a PUT is immediately visible to GET, HEAD and
// LIST, and a DELETE immediately invisible.
//
// covers: S-OBJ-01, S-OBJ-03
func TestReadAfterWrite(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "raw"
	e.mkBucket(bucket)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				key := fmt.Sprintf("w%d/k%02d", w, i)
				body := pseudoRandom(uint64(w*100+i), 1000+i*3000) // inline and on-disk sizes
				_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, body, getWith(t, e.ctx, e.root, bucket, key), key)
				head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
				if assert.NoError(t, err) {
					assert.Equal(t, int64(len(body)), aws.ToInt64(head.ContentLength))
				}
				list, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
				if assert.NoError(t, err) && assert.Len(t, list.Contents, 1, key) {
					assert.Equal(t, key, aws.ToString(list.Contents[0].Key))
				}
				// Overwrite and read back.
				_, err = e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("second")})
				if assert.NoError(t, err) {
					assert.Equal(t, []byte("second"), getWith(t, e.ctx, e.root, bucket, key))
				}
				if i%5 == 0 {
					_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
					assert.NoError(t, err)
					_, err = e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
					assert.Equal(t, http.StatusNotFound, httpStatus(err), key)
				}
			}
		}()
	}
	wg.Wait()
	list, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Len(t, list.Contents, 4*20)
}
