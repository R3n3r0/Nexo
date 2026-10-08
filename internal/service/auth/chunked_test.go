package auth_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// awsChunkedRequest builds the streaming PUT examples of the AWS
// "Signature Calculations: Transferring Payload in Multiple Chunks"
// documentation: 65536 + 1024 bytes of "a", without and with a trailing
// CRC32C checksum.
func awsChunkedRequest(withTrailer bool) (*http.Request, []byte) {
	payload := bytes.Repeat([]byte("a"), 66560)
	var body bytes.Buffer
	marker, signed, seed := "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		"content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class",
		"4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	sigs := []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	}
	if withTrailer {
		marker, signed, seed = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER",
			"content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class;x-amz-trailer",
			"106e2a8a18243abcf37539882f36619c00e2dfc72633413f02d3b74544bfeb8e"
		sigs = []string{
			"b474d8862b1487a5145d686f57f013e54db672cee1c953b3010fb58501ef5aa2",
			"1c1344b170168f8e65b41376b44b20fe354e373826ccbbe2c1d40a8cae51e5c7",
			"2ca2aba2005185cf7159c6277faf83795951dd77a3a99e6e65d5c9f85863f992",
		}
	}
	body.WriteString("10000;chunk-signature=" + sigs[0] + "\r\n")
	body.Write(payload[:65536])
	body.WriteString("\r\n400;chunk-signature=" + sigs[1] + "\r\n")
	body.Write(payload[65536:])
	body.WriteString("\r\n0;chunk-signature=" + sigs[2] + "\r\n")
	if withTrailer {
		body.WriteString("x-amz-checksum-crc32c:sOO8/Q==\r\n")
		body.WriteString("x-amz-trailer-signature:d81f82fc3505edab99d459891051a732e8730629a2e4a59689829ca17fe2e435\r\n")
	}
	body.WriteString("\r\n")

	req := httptest.NewRequest(http.MethodPut, "http://s3.amazonaws.com/examplebucket/chunkObject.txt", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Length", "66824")
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", "66560")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")
	req.Header.Set("x-amz-content-sha256", marker)
	req.Header.Set("x-amz-date", awsAmzDate)
	if withTrailer {
		req.Header.Set("x-amz-trailer", "x-amz-checksum-crc32c")
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+awsAccessKey+"/"+awsScope+",SignedHeaders="+signed+",Signature="+seed)
	return req, payload
}

func TestAWSDocumentationChunkedVectors(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime))
	for _, withTrailer := range []bool{false, true} {
		t.Run(fmt.Sprintf("trailer=%v", withTrailer), func(t *testing.T) {
			req, payload := awsChunkedRequest(withTrailer)
			id, err := v.Verify(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			assert.Equal(t, int64(66560), req.ContentLength, "the decoded length replaces the wire length")
			assert.Empty(t, req.Header.Get("Content-Encoding"), "aws-chunked is removed")
			data, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, data)
			if withTrailer {
				assert.Equal(t, "sOO8/Q==", req.Trailer.Get("x-amz-checksum-crc32c"))
				assert.Equal(t, "sOO8/Q==", req.Body.(interface{ Trailers() http.Header }).Trailers().Get("x-amz-checksum-crc32c"))
			}
			require.NoError(t, req.Body.Close())

			// Reading exactly the decoded length verifies everything.
			req, payload = awsChunkedRequest(withTrailer)
			_, err = v.Verify(ctx, req)
			require.NoError(t, err)
			buf := make([]byte, len(payload))
			_, err = io.ReadFull(req.Body, buf)
			require.NoError(t, err)
			assert.Equal(t, payload, buf)
			n, err := req.Body.Read(buf)
			assert.Zero(t, n)
			assert.ErrorIs(t, err, io.EOF)
		})
	}
}

// checksumOf computes an S3 additional checksum independently of the
// implementation.
func checksumOf(t *testing.T, algo string, data []byte) string {
	t.Helper()
	var h hash.Hash
	switch algo {
	case "crc32":
		h = crc32.NewIEEE()
	case "crc32c":
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "crc64nvme":
		table := crc64.MakeTable(0x9a6c9329ac4bc9b5)
		require.Equal(t, uint64(0xae8b14860a799888), crc64.Checksum([]byte("123456789"), table), "CRC-64/NVME check value")
		h = crc64.New(table)
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	}
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// streamingRequest signs a streaming upload with the Signer and returns it
// with the chunk encoder seeded by its signature.
func streamingRequest(t *testing.T, s auth.Signer, marker string, decoded int, trailer string) (*http.Request, *chunkEncoder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/bucket/key", nil)
	req.Header.Set("x-amz-decoded-content-length", fmt.Sprint(decoded))
	req.Header.Set("Content-Encoding", "aws-chunked")
	if trailer != "" {
		req.Header.Set("x-amz-trailer", trailer)
	}
	require.NoError(t, s.Sign(context.Background(), req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, marker))
	_, seed, _ := strings.Cut(req.Header.Get("Authorization"), "Signature=")
	return req, newChunkEncoder(awsSecretKey, testNow, seed, marker != "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
}

func setBody(req *http.Request, body []byte) *http.Request {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return req
}

func TestChunkedUploads(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	payload := bytes.Repeat([]byte("nexo"), 5000)

	t.Run("signed chunks", func(t *testing.T) {
		req, enc := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", len(payload), "")
		enc.chunk(payload[:8192]).chunk(payload[8192:16384]).chunk(payload[16384:]).final(false)
		_, err := v.Verify(ctx, setBody(req, enc.bytes()))
		require.NoError(t, err)
		got, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, got)
		assert.Equal(t, int64(len(payload)), req.ContentLength)
	})

	t.Run("empty payload", func(t *testing.T) {
		req, enc := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", 0, "")
		enc.final(false)
		_, err := v.Verify(ctx, setBody(req, enc.bytes()))
		require.NoError(t, err)
		got, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	for _, algo := range []string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256"} {
		name := "x-amz-checksum-" + algo
		sum := checksumOf(t, algo, payload)
		t.Run("signed trailer "+algo, func(t *testing.T) {
			req, enc := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", len(payload), name)
			enc.chunk(payload[:10000]).chunk(payload[10000:]).final(true).trailers(name, sum)
			_, err := v.Verify(ctx, setBody(req, enc.bytes()))
			require.NoError(t, err)
			got, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, got)
			assert.Equal(t, sum, req.Trailer.Get(name))
		})
		t.Run("unsigned trailer "+algo, func(t *testing.T) {
			req, enc := streamingRequest(t, s, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", len(payload), name)
			enc.chunk(payload[:7]).chunk(payload[7:]).final(true).trailers(name, sum)
			_, err := v.Verify(ctx, setBody(req, enc.bytes()))
			require.NoError(t, err)
			got, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, got)
			assert.Equal(t, sum, req.Trailer.Get(name))
		})
		t.Run("bad digest "+algo, func(t *testing.T) {
			req, enc := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", len(payload), name)
			enc.chunk(payload).final(true).trailers(name, checksumOf(t, algo, []byte("other")))
			_, err := v.Verify(ctx, setBody(req, enc.bytes()))
			require.NoError(t, err)
			_, err = io.ReadAll(req.Body)
			assertS3Error(t, err, domain.ErrBadDigest, "BadDigest")
			assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		})
	}

	t.Run("two checksums", func(t *testing.T) {
		req, enc := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", len(payload), "x-amz-checksum-sha256, x-amz-checksum-crc32")
		enc.chunk(payload).final(true).trailers("x-amz-checksum-sha256", checksumOf(t, "sha256", payload), "x-amz-checksum-crc32", checksumOf(t, "crc32", payload))
		_, err := v.Verify(ctx, setBody(req, enc.bytes()))
		require.NoError(t, err)
		_, err = io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Len(t, req.Trailer, 2)
	})

	t.Run("other content encodings are kept", func(t *testing.T) {
		req, _ := streamingRequest(t, s, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", len(payload), "")
		req.Header.Set("Content-Encoding", "aws-chunked, gzip")
		// Re-sign with the new header.
		require.NoError(t, s.Sign(ctx, req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"))
		_, seed, _ := strings.Cut(req.Header.Get("Authorization"), "Signature=")
		enc := newChunkEncoder(awsSecretKey, testNow, seed, true)
		enc.chunk(payload).final(false)
		_, err := v.Verify(ctx, setBody(req, enc.bytes()))
		require.NoError(t, err)
		assert.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	})
}

func TestChunkedErrors(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	payload := bytes.Repeat([]byte("data"), 3000)
	crc := checksumOf(t, "crc32", payload)
	const (
		signedMarker   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
		trailerMarker  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
		unsignedMarker = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
		crcName        = "x-amz-checksum-crc32"
	)
	tests := []struct {
		name     string
		marker   string
		decoded  int
		trailer  string
		body     func(enc *chunkEncoder) []byte
		sentinel error
		code     string
	}{
		{"tampered chunk data", signedMarker, len(payload), "", func(enc *chunkEncoder) []byte {
			return bytes.Replace(enc.chunk(payload).final(false).bytes(), []byte("datadata"), []byte("DATAdata"), 1)
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"chunks reordered", signedMarker, len(payload), "", func(enc *chunkEncoder) []byte {
			first := newChunkEncoder(awsSecretKey, testNow, enc.prevSig, true)
			first.chunk(payload[:100])
			second := newChunkEncoder(awsSecretKey, testNow, enc.prevSig, true)
			second.chunk(payload[100:])
			return append(second.bytes(), first.bytes()...)
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"tampered final chunk signature", signedMarker, len(payload), "", func(enc *chunkEncoder) []byte {
			enc.chunk(payload)
			enc.prevSig = strings.Repeat("0", 64)
			return enc.final(false).bytes()
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"tampered trailer value", trailerMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			return bytes.Replace(enc.chunk(payload).final(true).trailers(crcName, crc).bytes(), []byte(crc), []byte("AAAAAA=="), 1)
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"missing trailer signature", trailerMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			enc.chunk(payload).final(true)
			enc.buf.WriteString(crcName + ":" + crc + "\r\n\r\n")
			return enc.bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"duplicate trailer signature", trailerMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			enc.chunk(payload).final(true)
			enc.buf.WriteString("x-amz-trailer-signature:a\r\nx-amz-trailer-signature:b\r\n\r\n")
			return enc.bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"missing declared trailer", unsignedMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			return enc.chunk(payload).final(true).trailers().bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"undeclared trailer", unsignedMarker, len(payload), "", func(enc *chunkEncoder) []byte {
			return enc.chunk(payload).final(true).trailers(crcName, crc).bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"duplicate trailer", unsignedMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			return enc.chunk(payload).final(true).trailers(crcName, crc, crcName, crc).bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"malformed trailer line", unsignedMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			enc.chunk(payload).final(true)
			enc.buf.WriteString("no colon here\r\n\r\n")
			return enc.bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"too many trailers", unsignedMarker, len(payload), crcName, func(enc *chunkEncoder) []byte {
			enc.chunk(payload).final(true)
			for i := 0; i < 20; i++ {
				fmt.Fprintf(&enc.buf, "x-amz-checksum-crc32:%d\r\n", i)
			}
			enc.buf.WriteString("\r\n")
			return enc.bytes()
		}, domain.ErrInvalidArgument, "MalformedTrailerError"},
		{"decoded length too short", signedMarker, len(payload) - 1, "", func(enc *chunkEncoder) []byte {
			return enc.chunk(payload).final(false).bytes()
		}, domain.ErrInvalidArgument, "IncompleteBody"},
		{"decoded length too long", signedMarker, len(payload) + 1, "", func(enc *chunkEncoder) []byte {
			return enc.chunk(payload).final(false).bytes()
		}, domain.ErrInvalidArgument, "IncompleteBody"},
		{"data after the declared length", signedMarker, 100, "", func(enc *chunkEncoder) []byte {
			return enc.chunk(payload[:100]).chunk(payload[100:200]).final(false).bytes()
		}, domain.ErrInvalidArgument, "IncompleteBody"},
		{"invalid chunk size", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("zz\r\nabc\r\n0\r\n\r\n") }, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"empty chunk size", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte(";x\r\nabc\r\n0\r\n\r\n") }, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"chunk size overflow", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("10000000000000000\r\nabc\r\n0\r\n\r\n") }, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"missing CRLF after data", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3\r\nabcXX0\r\n\r\n") }, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"truncated chunk", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3\r\nab") }, domain.ErrInvalidArgument, "IncompleteBody"},
		{"truncated header", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3") }, domain.ErrInvalidArgument, "IncompleteBody"},
		{"truncated terminator", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3\r\nabc") }, domain.ErrInvalidArgument, "IncompleteBody"},
		{"truncated after CR", unsignedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3\r\nabc\r") }, domain.ErrInvalidArgument, "IncompleteBody"},
		{"no final chunk", unsignedMarker, 6, "", func(*chunkEncoder) []byte { return []byte("3\r\nabc\r\n") }, domain.ErrInvalidArgument, "IncompleteBody"},
		{"missing chunk signature", signedMarker, 3, "", func(*chunkEncoder) []byte { return []byte("3\r\nabc\r\n0\r\n\r\n") }, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"short chunk signature", signedMarker, 3, "", func(*chunkEncoder) []byte {
			return []byte("3;chunk-signature=short\r\nabc\r\n0\r\n\r\n")
		}, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
		{"line too long", unsignedMarker, 3, "", func(*chunkEncoder) []byte {
			return []byte("3;" + strings.Repeat("x", 70000) + "\r\nabc\r\n0\r\n\r\n")
		}, domain.ErrInvalidArgument, "InvalidChunkEncoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, enc := streamingRequest(t, s, tt.marker, tt.decoded, tt.trailer)
			_, err := v.Verify(ctx, setBody(req, tt.body(enc)))
			require.NoError(t, err)
			_, err = io.ReadAll(req.Body)
			assertS3Error(t, err, tt.sentinel, tt.code)
			n, again := req.Body.Read(make([]byte, 8))
			assert.Zero(t, n)
			assert.Equal(t, err, again, "the error is sticky")
		})
		t.Run(tt.name+" (exact read)", func(t *testing.T) {
			req, enc := streamingRequest(t, s, tt.marker, tt.decoded, tt.trailer)
			_, err := v.Verify(ctx, setBody(req, tt.body(enc)))
			require.NoError(t, err)
			buf := make([]byte, tt.decoded)
			_, err = io.ReadFull(req.Body, buf)
			assert.Error(t, err, "reading exactly the decoded length never hides a failed check")
		})
	}

	t.Run("upfront errors", func(t *testing.T) {
		req, _ := streamingRequest(t, s, signedMarker, 3, "")
		req.Header.Del("x-amz-decoded-content-length")
		req.Body = io.NopCloser(strings.NewReader("x"))
		_, err := v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrSignatureMismatch, "SignatureDoesNotMatch", "the decoded length is signed")

		req = httptest.NewRequest(http.MethodPut, "http://localhost:9000/bucket/key", strings.NewReader("x"))
		require.NoError(t, s.Sign(ctx, req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, signedMarker))
		_, err = v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrInvalidArgument, "MissingContentLength")

		req, _ = streamingRequest(t, s, signedMarker, 3, "")
		_, err = v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrInvalidArgument, "IncompleteBody", "no body")

		req, _ = streamingRequest(t, s, unsignedMarker, 3, "x-amz-checksum-md5")
		_, err = v.Verify(ctx, setBody(req, []byte("3\r\nabc\r\n0\r\n\r\n")))
		assertS3Error(t, err, domain.ErrInvalidArgument, "MalformedTrailerError", "unsupported trailer")
	})

	t.Run("lenient framing", func(t *testing.T) {
		// Bare LF line endings and a missing final empty line are accepted.
		for _, body := range []string{"3\nabc\n0\n\n", "3\r\nabc\r\n0\r\n", "3\r\nabc\r\n0\r\n\n"} {
			req, _ := streamingRequest(t, s, unsignedMarker, 3, "")
			_, err := v.Verify(ctx, setBody(req, []byte(body)))
			require.NoError(t, err)
			got, err := io.ReadAll(req.Body)
			require.NoError(t, err, "%q", body)
			assert.Equal(t, "abc", string(got))
		}
	})

	t.Run("zero-length reads", func(t *testing.T) {
		req, enc := streamingRequest(t, s, signedMarker, 4, "")
		_, err := v.Verify(ctx, setBody(req, enc.chunk([]byte("abcd")).final(false).bytes()))
		require.NoError(t, err)
		n, err := req.Body.Read(nil)
		assert.Zero(t, n)
		require.NoError(t, err)
		got, err := io.ReadAll(iotestOneByte{req.Body})
		require.NoError(t, err)
		assert.Equal(t, "abcd", string(got))
	})
}

// iotestOneByte reads one byte at a time.
type iotestOneByte struct{ r io.Reader }

func (o iotestOneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestAnonymousUnsignedTrailerUpload(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(testNow))
	payload := bytes.Repeat([]byte("public"), 3000)
	const name = "x-amz-checksum-crc32"
	anonymousRequest := func(decoded string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/public/key", nil)
		req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", decoded)
		req.Header.Set("X-Amz-Trailer", name)
		return req
	}
	tests := []struct {
		name    string
		decoded string
		sum     string
		verify  func(t *testing.T, req *http.Request, verr error)
	}{
		{
			name: "decoded and verified", decoded: fmt.Sprint(len(payload)), sum: checksumOf(t, "crc32", payload),
			verify: func(t *testing.T, req *http.Request, verr error) {
				require.NoError(t, verr)
				got, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, payload, got)
				assert.Equal(t, int64(len(payload)), req.ContentLength)
				assert.Empty(t, req.Header.Get("Content-Encoding"))
				assert.Equal(t, checksumOf(t, "crc32", payload), req.Trailer.Get(name))
			},
		},
		{
			name: "bad trailing checksum", decoded: fmt.Sprint(len(payload)), sum: checksumOf(t, "crc32", []byte("other")),
			verify: func(t *testing.T, req *http.Request, verr error) {
				require.NoError(t, verr)
				_, err := io.ReadAll(req.Body)
				assertS3Error(t, err, domain.ErrBadDigest, "BadDigest")
			},
		},
		{
			name: "missing decoded length", decoded: "", sum: checksumOf(t, "crc32", payload),
			verify: func(t *testing.T, _ *http.Request, verr error) {
				assertS3Error(t, verr, domain.ErrInvalidArgument, "MissingContentLength")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := newChunkEncoder(awsSecretKey, testNow, "", false)
			enc.chunk(payload[:1000]).chunk(payload[1000:]).final(true).trailers(name, tc.sum)
			req := setBody(anonymousRequest(tc.decoded), enc.bytes())
			id, err := v.Verify(ctx, req)
			if err == nil {
				assert.True(t, id.IsAnonymous)
				assert.Equal(t, domain.AuthTypeAnonymous, id.AuthType)
			}
			tc.verify(t, req, err)
		})
	}
}
