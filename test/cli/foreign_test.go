package cli_test

import (
	"crypto/md5" //nolint:gosec // G501: the ETag of S3 is an MD5, not a security primitive.
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

// foreignModTime is the modification time of the objects of foreigns3.
var foreignModTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// cmdForeignS3 implements "foreigns3 VAR BUCKET KEY...": it starts a
// minimal S3 endpoint (ListBuckets, ListObjectsV2, Head/GetObject, any
// credentials) serving BUCKET with the objects KEY (content "data of
// KEY") and sets VAR to its URL. Unlike Nexo it accepts any key, like AWS
// S3 does ("../../x" included), so the scripts can check how nexoctl
// handles names it must not trust.
func cmdForeignS3(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 3 {
		ts.Fatalf("usage: foreigns3 VAR BUCKET KEY...")
	}
	bucket, keys := args[1], append([]string(nil), args[2:]...)
	sort.Strings(keys)
	objects := make(map[string]string, len(keys))
	for _, k := range keys {
		objects[k] = "data of " + k
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		b, key, _ := strings.Cut(path, "/")
		switch {
		case path == "" && r.Method == http.MethodGet:
			writeXML(w, listAllMyBuckets{Buckets: []foreignBucket{{Name: bucket, CreationDate: foreignModTime}}})
		case b != bucket:
			http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			prefix := r.URL.Query().Get("prefix")
			res := listBucketResult{Name: bucket, Prefix: prefix, MaxKeys: 1000}
			for _, k := range keys {
				if strings.HasPrefix(k, prefix) {
					res.Contents = append(res.Contents, foreignObject{Key: k, LastModified: foreignModTime, ETag: etagOf(objects[k]), Size: len(objects[k]), StorageClass: "STANDARD"})
				}
			}
			res.KeyCount = len(res.Contents)
			writeXML(w, res)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			data, ok := objects[key]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", etagOf(data))
			w.Header().Set("Last-Modified", foreignModTime.Format(http.TimeFormat))
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(data))
			}
		default:
			http.Error(w, "<Error><Code>NotImplemented</Code></Error>", http.StatusNotImplemented)
		}
	}))
	ts.Defer(srv.Close)
	ts.Setenv(args[0], srv.URL)
}

func etagOf(data string) string {
	sum := md5.Sum([]byte(data)) //nolint:gosec // G401: see the import.
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

type listAllMyBuckets struct {
	XMLName xml.Name        `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Buckets []foreignBucket `xml:"Buckets>Bucket"`
}

type foreignBucket struct {
	Name         string
	CreationDate time.Time
}

type listBucketResult struct {
	XMLName     xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name        string
	Prefix      string
	KeyCount    int
	MaxKeys     int
	IsTruncated bool
	Contents    []foreignObject
}

type foreignObject struct {
	Key          string
	LastModified time.Time
	ETag         string
	Size         int
	StorageClass string
}
