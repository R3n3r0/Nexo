package s3select_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/s3select"
)

// collect is a Sink keeping the records.
type collect struct {
	buf      bytes.Buffer
	chunks   int
	progress int
}

func (c *collect) Records(p []byte) error {
	c.chunks++
	c.buf.Write(p)
	return nil
}

func (c *collect) Progress(s3select.Stats) error {
	c.progress++
	return nil
}

const people = `name,age,city,score
Alice,30,Rome,8.5
Bob,25,Milan,7
"Carol, Jr.",41,Rome,9.25
Dan,,Turin,6
`

func csvReq(sql, header string) s3select.Request {
	return s3select.Request{
		Expression: sql, ExpressionType: "SQL",
		Input:  s3select.InputSerialization{CSV: &s3select.CSVInput{FileHeaderInfo: header}},
		Output: s3select.OutputSerialization{CSV: &s3select.CSVOutput{}},
	}
}

func run(t *testing.T, req s3select.Request, content string) (string, s3select.Stats) {
	t.Helper()
	svc := s3select.New(s3select.Deps{})
	require.NoError(t, svc.Validate(req))
	var sink collect
	stats, err := svc.Run(context.Background(), req, strings.NewReader(content), &sink)
	require.NoError(t, err)
	return sink.buf.String(), stats
}

func TestCSVQueries(t *testing.T) {
	cases := []struct {
		sql, header, want string
	}{
		{"SELECT * FROM S3Object", "USE", "Alice,30,Rome,8.5\nBob,25,Milan,7\n\"Carol, Jr.\",41,Rome,9.25\nDan,,Turin,6\n"},
		{"SELECT s.name FROM S3Object s WHERE s.city = 'Rome'", "USE", "Alice\n\"Carol, Jr.\"\n"},
		{"SELECT name, age FROM S3Object WHERE age > 28", "USE", "Alice,30\n\"Carol, Jr.\",41\n"},
		{"SELECT s._1 FROM S3Object s WHERE CAST(s._2 AS INT) < 30", "IGNORE", "Bob\n"},
		{"SELECT s._1, s._3 FROM S3Object s LIMIT 1", "NONE", "name,city\n"},
		{"SELECT COUNT(*) FROM S3Object", "USE", "4\n"},
		{"SELECT COUNT(age), SUM(age), MIN(age), MAX(age), AVG(score) FROM S3Object", "USE", "4,96,25,41,7.6875\n"},
		{"SELECT UPPER(name) FROM S3Object WHERE name LIKE 'C%'", "USE", "\"CAROL, JR.\"\n"},
		{"SELECT name FROM S3Object WHERE age IS NULL OR age = ''", "USE", "Dan\n"},
		{"SELECT name FROM S3Object WHERE city IN ('Milan', 'Turin') AND NOT name = 'Dan'", "USE", "Bob\n"},
		{"SELECT name FROM S3Object WHERE age BETWEEN 26 AND 40", "USE", "Alice\n"},
		{"SELECT age * 2 + 1, name || '!' FROM S3Object WHERE name = 'Bob'", "USE", "51,Bob!\n"},
		{"SELECT SUBSTRING(city FROM 2 FOR 3), CHAR_LENGTH(city) FROM S3Object WHERE name = 'Alice'", "USE", "ome,4\n"},
		{"SELECT name FROM S3Object WHERE score >= 8.5", "USE", "Alice\n\"Carol, Jr.\"\n"},
		{"SELECT COALESCE(NULLIF(age, ''), 'unknown') FROM S3Object WHERE city = 'Turin'", "USE", "unknown\n"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			got, _ := run(t, csvReq(c.sql, c.header), people)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestJSONQueries(t *testing.T) {
	lines := `{"id":1,"user":{"name":"ann","tags":["a","b"]},"amount":12.5}
{"id":2,"user":{"name":"ben","tags":[]},"amount":3}
{"id":3,"user":{"name":"cid"},"amount":null}
`
	jsonReq := func(sql, typ string, csvOut bool) s3select.Request {
		r := s3select.Request{Expression: sql, ExpressionType: "SQL",
			Input:  s3select.InputSerialization{JSON: &s3select.JSONInput{Type: typ}},
			Output: s3select.OutputSerialization{JSON: &s3select.JSONOutput{}}}
		if csvOut {
			r.Output = s3select.OutputSerialization{CSV: &s3select.CSVOutput{}}
		}
		return r
	}
	got, _ := run(t, jsonReq("SELECT * FROM S3Object[*] s WHERE s.id > 1", "LINES", false), lines)
	assert.Equal(t, `{"id":2,"user":{"name":"ben","tags":[]},"amount":3}`+"\n"+`{"id":3,"user":{"name":"cid"},"amount":null}`+"\n", got)
	got, _ = run(t, jsonReq("SELECT s.user.name AS who, s.user.tags[1] FROM S3Object[*] s", "LINES", false), lines)
	assert.Equal(t, `{"who":"ann","_2":"b"}`+"\n"+`{"who":"ben"}`+"\n"+`{"who":"cid"}`+"\n", got, "a missing value is left out")
	got, _ = run(t, jsonReq("SELECT s.id FROM S3Object[*] s WHERE s.amount IS NULL", "LINES", true), lines)
	assert.Equal(t, "3\n", got)
	got, _ = run(t, jsonReq("SELECT s.id FROM S3Object[*] s WHERE s.user.tags IS MISSING", "LINES", true), lines)
	assert.Equal(t, "3\n", got)
	got, _ = run(t, jsonReq("SELECT SUM(s.amount), COUNT(s.amount) FROM S3Object[*] s", "LINES", true), lines)
	assert.Equal(t, "15.5,2\n", got)
	got, _ = run(t, jsonReq("SELECT * FROM S3Object[*] s", "DOCUMENT", true), `[{"b":1,"a":"x"},{"b":2,"a":"y,z"}]`)
	assert.Equal(t, "1,x\n2,\"y,z\"\n", got, "the elements of a document array are records, in the order of their keys")
	got, _ = run(t, jsonReq("SELECT o.n FROM S3Object[*].orders o WHERE o.n > 1", "DOCUMENT", true), `{"orders":[{"n":1},{"n":2},{"n":3}]}`)
	assert.Equal(t, "2\n3\n", got)
}

func TestCSVToJSONAndSerializations(t *testing.T) {
	req := csvReq("SELECT * FROM S3Object WHERE city = 'Milan'", "USE")
	req.Output = s3select.OutputSerialization{JSON: &s3select.JSONOutput{RecordDelimiter: ";"}}
	got, _ := run(t, req, people)
	assert.Equal(t, `{"name":"Bob","age":"25","city":"Milan","score":"7"};`, got)

	req = csvReq("SELECT name, city FROM S3Object LIMIT 2", "USE")
	req.Input.CSV.FieldDelimiter = ";"
	req.Input.CSV.RecordDelimiter = "|"
	req.Output.CSV = &s3select.CSVOutput{FieldDelimiter: "\t", QuoteFields: "ALWAYS", RecordDelimiter: "\r\n"}
	got, _ = run(t, req, "name;city|ann;Rome|ben;Milan|cid;Turin|")
	assert.Equal(t, "\"ann\"\t\"Rome\"\r\n\"ben\"\t\"Milan\"\r\n", got)

	req = csvReq("SELECT _1 FROM S3Object", "NONE")
	req.Input.CSV.Comments = "#"
	got, _ = run(t, req, "# a comment\nx\ny\n")
	assert.Equal(t, "x\ny\n", got)
}

func TestCompressedInputAndStats(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	var plain strings.Builder
	plain.WriteString("n\n")
	for i := range 50000 {
		fmt.Fprintf(&plain, "%d\n", i)
	}
	_, err := zw.Write([]byte(plain.String()))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	req := csvReq("SELECT n FROM S3Object WHERE CAST(n AS INT) % 10000 = 0", "USE")
	req.Input.CompressionType = "GZIP"
	req.RequestProgress = true
	svc := s3select.New(s3select.Deps{})
	var sink collect
	stats, err := svc.Run(context.Background(), req, bytes.NewReader(gz.Bytes()), &sink)
	require.NoError(t, err)
	assert.Equal(t, "0\n10000\n20000\n30000\n40000\n", sink.buf.String())
	assert.Equal(t, int64(gz.Len()), stats.BytesScanned)
	assert.Equal(t, int64(plain.Len()), stats.BytesProcessed)
	assert.Equal(t, int64(sink.buf.Len()), stats.BytesReturned)
	assert.Equal(t, 1, sink.progress)

	// A large output is handed in chunks.
	req = csvReq("SELECT n FROM S3Object", "USE")
	req.Input.CompressionType = "GZIP"
	sink = collect{}
	_, err = svc.Run(context.Background(), req, bytes.NewReader(gz.Bytes()), &sink)
	require.NoError(t, err)
	assert.Greater(t, sink.chunks, 1)
	assert.Equal(t, plain.String()[2:], sink.buf.String())
}

func TestInvalidRequests(t *testing.T) {
	svc := s3select.New(s3select.Deps{})
	cases := []struct {
		name string
		req  s3select.Request
		code string
	}{
		{"expression type", s3select.Request{Expression: "SELECT * FROM S3Object", ExpressionType: "XPATH"}, "InvalidExpressionType"},
		{"syntax", csvReq("SELECT FROM S3Object", "USE"), "ParseUnexpectedToken"},
		{"trailing tokens", csvReq("SELECT * FROM S3Object s WHERE", "USE"), "ParseUnexpectedToken"},
		{"from", csvReq("SELECT * FROM table1", "USE"), "ParseUnexpectedToken"},
		{"unknown function", csvReq("SELECT md5(name) FROM S3Object", "USE"), "UnsupportedSyntax"},
		{"aggregate with column", csvReq("SELECT name, COUNT(*) FROM S3Object", "USE"), "UnsupportedSyntax"},
		{"aggregate in where", csvReq("SELECT name FROM S3Object WHERE COUNT(*) > 1", "USE"), "UnsupportedSyntax"},
		{"header info", csvReq("SELECT * FROM S3Object", "FIRST"), "InvalidFileHeaderInfo"},
		{"scan range", func() s3select.Request {
			r := csvReq("SELECT * FROM S3Object", "USE")
			r.ScanRange = &s3select.ScanRange{}
			return r
		}(), "NotImplemented"},
		{"parquet", s3select.Request{Expression: "SELECT * FROM S3Object", ExpressionType: "SQL",
			Input: s3select.InputSerialization{Parquet: true}, Output: s3select.OutputSerialization{CSV: &s3select.CSVOutput{}}}, "NotImplemented"},
		{"nesting", csvReq("SELECT "+strings.Repeat("(", 1000)+"1"+strings.Repeat(")", 1000)+" FROM S3Object", "USE"), "UnsupportedSyntax"},
		{"negations", csvReq("SELECT * FROM S3Object WHERE "+strings.Repeat("NOT ", 1000)+"true", "USE"), "UnsupportedSyntax"},
		{"length", csvReq("SELECT * FROM S3Object WHERE a = '"+strings.Repeat("x", 300<<10)+"'", "USE"), "ExpressionTooLong"},
		{"compression", func() s3select.Request {
			r := csvReq("SELECT * FROM S3Object", "USE")
			r.Input.CompressionType = "ZSTD"
			return r
		}(), "InvalidCompressionFormat"},
	}
	for _, c := range cases {
		err := svc.Validate(c.req)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, c.name)
		var s3err *domain.S3Error
		require.True(t, errors.As(err, &s3err), c.name)
		assert.Equal(t, c.code, s3err.Code, c.name)
	}

	// Evaluation errors are reported by Run.
	var sink collect
	_, err := svc.Run(context.Background(), csvReq("SELECT CAST(name AS INT) FROM S3Object", "USE"), strings.NewReader(people), &sink)
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "CastFailed", s3err.Code)
	_, err = svc.Run(context.Background(), csvReq("SELECT age / 0 FROM S3Object", "USE"), strings.NewReader(people), &sink)
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "DivisionByZero", s3err.Code)
}

func TestLikePatterns(t *testing.T) {
	for _, c := range []struct {
		value, pattern string
		match          bool
	}{
		{"abc", "a%", true}, {"abc", "%c", true}, {"abc", "a_c", true}, {"abc", "a_", false},
		{"a%c", `a\%c`, true}, {"abc", `a\%c`, false}, {"x", "%", true}, {"x", "", false},
	} {
		req := csvReq(fmt.Sprintf(`SELECT _1 FROM S3Object WHERE _1 LIKE '%s' ESCAPE '\'`, c.pattern), "NONE")
		got, _ := run(t, req, c.value+"\n")
		assert.Equal(t, c.match, got != "", "%q LIKE %q", c.value, c.pattern)
	}
}

// TestLikeIsLinear: a pattern with many % against a long value answers at
// once (no exponential backtracking).
func TestLikeIsLinear(t *testing.T) {
	value := strings.Repeat("a", 5000)
	req := csvReq("SELECT _1 FROM S3Object WHERE _1 LIKE '"+strings.Repeat("%a", 30)+"%b'", "NONE")
	got, _ := run(t, req, value+"\n")
	assert.Empty(t, got)
	req = csvReq("SELECT _1 FROM S3Object WHERE _1 LIKE '"+strings.Repeat("%a", 30)+"%'", "NONE")
	got, _ = run(t, req, value+"\n")
	assert.Equal(t, value+"\n", got)
}

// TestOverlongRecord: a line beyond 1 MiB fails instead of being buffered.
func TestOverlongRecord(t *testing.T) {
	svc := s3select.New(s3select.Deps{})
	var sink collect
	_, err := svc.Run(context.Background(), csvReq("SELECT * FROM S3Object", "NONE"), strings.NewReader(strings.Repeat("x", 2<<20)), &sink)
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "OverMaxRecordSize", s3err.Code)
}
