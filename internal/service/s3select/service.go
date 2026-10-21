// Package s3select evaluates the SQL queries of SelectObjectContent
// (S-OBJ-16) over the content of an object: CSV or JSON records, plain or
// compressed with GZIP or BZIP2, filtered and projected by a subset of the
// S3 Select SQL, written as CSV or JSON records.
//
// The supported SQL is SELECT <projection> FROM S3Object[*] [alias]
// [WHERE <condition>] [LIMIT <n>], where the projection is * or a list of
// expressions with an optional alias, or of aggregates (COUNT, SUM, AVG,
// MIN, MAX). The expressions are paths (s.name, s._1, s.a.b[0], quoted
// names), literals, arithmetic (+ - * / %), the string concatenation ||,
// comparisons, AND/OR/NOT, LIKE (with ESCAPE), BETWEEN, IN, IS [NOT] NULL
// and IS [NOT] MISSING, CAST(<expr> AS INT|FLOAT|DECIMAL|STRING|BOOL),
// LOWER, UPPER, CHAR_LENGTH, TRIM, SUBSTRING, COALESCE and NULLIF. CSV
// fields are strings: a comparison or an arithmetic operation with a
// number reads them as numbers when they parse as one.
package s3select

import (
	"context"
	"io"
)

// Service evaluates S3 Select requests.
type Service interface {
	// Validate checks a request (its SQL and serializations) before the
	// object is read: an invalid request is a domain.S3Error with the
	// code of S3 (ParseUnexpectedToken, InvalidExpressionType...).
	Validate(req Request) error
	// Run evaluates req over src, the content of the object (compressed
	// as req.Input.CompressionType says), and hands the serialized
	// records to sink. It returns the statistics of the run; an error
	// after records were handed is reported by the caller as an error
	// event.
	Run(ctx context.Context, req Request, src io.Reader, sink Sink) (Stats, error)
}

// Sink receives the output of a run: chunks of serialized records, in
// order, and the progress when the request asks for it.
type Sink interface {
	Records(p []byte) error
	Progress(s Stats) error
}

// Request is a SelectObjectContent request.
type Request struct {
	Expression     string
	ExpressionType string
	Input          InputSerialization
	Output         OutputSerialization
	// RequestProgress asks for Progress events.
	RequestProgress bool
	// ScanRange, when set, is not supported (Validate refuses it).
	ScanRange *ScanRange
}

// ScanRange is the byte range of the object a request scans.
type ScanRange struct {
	Start, End *int64
}

// InputSerialization describes the content of the object.
type InputSerialization struct {
	// CompressionType is NONE (or empty), GZIP or BZIP2.
	CompressionType string
	CSV             *CSVInput
	JSON            *JSONInput
	// Parquet is set for a Parquet request, which is not supported.
	Parquet bool
}

// CSVInput is the CSV format of the content.
type CSVInput struct {
	// FileHeaderInfo is NONE (default), USE (the first line names the
	// columns) or IGNORE (the first line is skipped).
	FileHeaderInfo             string
	Comments                   string
	QuoteEscapeCharacter       string
	RecordDelimiter            string
	FieldDelimiter             string
	QuoteCharacter             string
	AllowQuotedRecordDelimiter bool
}

// JSONInput is the JSON format of the content: DOCUMENT or LINES.
type JSONInput struct {
	Type string
}

// OutputSerialization describes the format of the output records.
type OutputSerialization struct {
	CSV  *CSVOutput
	JSON *JSONOutput
}

// CSVOutput is the CSV format of the output.
type CSVOutput struct {
	// QuoteFields is ASNEEDED (default) or ALWAYS.
	QuoteFields          string
	QuoteEscapeCharacter string
	RecordDelimiter      string
	FieldDelimiter       string
	QuoteCharacter       string
}

// JSONOutput is the JSON format of the output.
type JSONOutput struct {
	RecordDelimiter string
}

// Stats are the counters of a run: the bytes of the object read, the bytes
// of content they held (decompressed) and the bytes of output.
type Stats struct {
	BytesScanned   int64
	BytesProcessed int64
	BytesReturned  int64
}
