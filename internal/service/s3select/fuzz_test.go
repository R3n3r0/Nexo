package s3select_test

import (
	"context"
	"strings"
	"testing"

	"github.com/r3n3r0/nexo/internal/service/s3select"
)

// FuzzSelect: any query over any content fails cleanly or runs, never
// panics.
func FuzzSelect(f *testing.F) {
	f.Add("SELECT * FROM S3Object", "a,b\n1,2\n", false)
	f.Add("SELECT s.a, COUNT(*) FROM S3Object s WHERE s.b LIKE '%x_' ESCAPE '\\'", "a,b\nx,yx1\n", false)
	f.Add("SELECT SUBSTRING(s.a FROM -2 FOR 3), CAST(s.b AS FLOAT) / 0 FROM S3Object[*] s LIMIT 2", `{"a":"hello","b":[1,{"c":2}]}`, true)
	f.Add("SELECT TRIM(LEADING 'x' FROM a) || UPPER(b) FROM S3Object WHERE a IN (1, 'x') AND NOT b IS MISSING", "a,b\n", false)
	svc := s3select.New(s3select.Deps{})
	f.Fuzz(func(t *testing.T, sql, content string, jsonInput bool) {
		req := s3select.Request{Expression: sql, ExpressionType: "SQL", Output: s3select.OutputSerialization{JSON: &s3select.JSONOutput{}}}
		if jsonInput {
			req.Input.JSON = &s3select.JSONInput{Type: "DOCUMENT"}
		} else {
			req.Input.CSV = &s3select.CSVInput{FileHeaderInfo: "USE"}
		}
		if svc.Validate(req) != nil {
			return
		}
		_, _ = svc.Run(context.Background(), req, strings.NewReader(content), discard{})
	})
}

type discard struct{}

func (discard) Records([]byte) error          { return nil }
func (discard) Progress(s3select.Stats) error { return nil }
