package s3select

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Deps are the dependencies of the service.
type Deps struct {
	Logger *slog.Logger
}

type service struct {
	deps Deps
}

var _ Service = (*service)(nil)

// New returns the S3 Select service.
func New(deps Deps) Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &service{deps: deps}
}

const (
	// outputChunk is the size of the records handed to the sink at once.
	outputChunk = 64 << 10
	// maxRecordSize bounds a record (a CSV line, a JSON value), as S3.
	maxRecordSize = 1 << 20
	// maxExpression bounds the SQL of a request, as S3.
	maxExpression = 256 << 10
)

// Validate implements Service.
func (s *service) Validate(req Request) error {
	_, err := prepare(req)
	return err
}

// prepared is a validated request.
type prepared struct {
	q   *query
	req Request
}

func prepare(req Request) (*prepared, error) {
	if !strings.EqualFold(req.ExpressionType, "SQL") {
		return nil, requestError("InvalidExpressionType", "the ExpressionType must be SQL")
	}
	if len(req.Expression) > maxExpression {
		return nil, requestError("ExpressionTooLong", "the Expression exceeds %d bytes", maxExpression)
	}
	if strings.TrimSpace(req.Expression) == "" {
		return nil, requestError("MissingRequiredParameter", "the Expression is empty")
	}
	if req.ScanRange != nil {
		return nil, requestError("NotImplemented", "ScanRange is not supported")
	}
	in := req.Input
	switch {
	case in.Parquet:
		return nil, requestError("NotImplemented", "Parquet input is not supported")
	case (in.CSV == nil) == (in.JSON == nil):
		return nil, requestError("InvalidRequestParameter", "the InputSerialization names one of CSV and JSON")
	}
	switch strings.ToUpper(in.CompressionType) {
	case "", "NONE", "GZIP", "BZIP2":
	default:
		return nil, requestError("InvalidCompressionFormat", "the CompressionType %q is not supported", in.CompressionType)
	}
	if c := in.CSV; c != nil {
		switch strings.ToUpper(c.FileHeaderInfo) {
		case "", "NONE", "USE", "IGNORE":
		default:
			return nil, requestError("InvalidFileHeaderInfo", "the FileHeaderInfo %q is not supported", c.FileHeaderInfo)
		}
		if err := singleChar("FieldDelimiter", c.FieldDelimiter); err != nil {
			return nil, err
		}
		if err := singleChar("Comments", c.Comments); err != nil {
			return nil, err
		}
		if c.QuoteCharacter != "" && c.QuoteCharacter != `"` {
			return nil, requestError("InvalidQuoteFields", "only the \" QuoteCharacter is supported")
		}
		if c.QuoteEscapeCharacter != "" && c.QuoteEscapeCharacter != `"` {
			return nil, requestError("InvalidQuoteFields", "only the \" QuoteEscapeCharacter is supported")
		}
		switch d := c.RecordDelimiter; {
		case d == "", d == "\n", d == "\r\n", len(d) == 1:
		default:
			return nil, requestError("InvalidRequestParameter", "the RecordDelimiter is one character or \\r\\n")
		}
	}
	if j := in.JSON; j != nil {
		switch strings.ToUpper(j.Type) {
		case "DOCUMENT", "LINES":
		default:
			return nil, requestError("InvalidJsonType", "the JSON Type is DOCUMENT or LINES")
		}
	}
	out := req.Output
	if (out.CSV == nil) == (out.JSON == nil) {
		return nil, requestError("InvalidRequestParameter", "the OutputSerialization names one of CSV and JSON")
	}
	if c := out.CSV; c != nil {
		switch strings.ToUpper(c.QuoteFields) {
		case "", "ASNEEDED", "ALWAYS":
		default:
			return nil, requestError("InvalidQuoteFields", "the QuoteFields %q is not supported", c.QuoteFields)
		}
	}
	q, err := parseQuery(req.Expression)
	if err != nil {
		return nil, err
	}
	if len(q.from) > 0 && in.CSV != nil {
		return nil, unsupported("a path after S3Object applies to JSON")
	}
	return &prepared{q: q, req: req}, nil
}

func singleChar(name, v string) error {
	if v != "" && utf8.RuneCountInString(v) != 1 {
		return requestError("InvalidRequestParameter", "the %s is one character", name)
	}
	return nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Run implements Service.
func (s *service) Run(ctx context.Context, req Request, src io.Reader, sink Sink) (Stats, error) {
	p, err := prepare(req)
	if err != nil {
		return Stats{}, err
	}
	scanned := &countingReader{r: src}
	var content io.Reader = scanned
	switch strings.ToUpper(req.Input.CompressionType) {
	case "GZIP":
		zr, err := gzip.NewReader(scanned)
		if err != nil {
			return Stats{BytesScanned: scanned.n}, requestError("InvalidCompressionFormat", "the content is not GZIP: %v", err)
		}
		defer func() { _ = zr.Close() }()
		content = zr
	case "BZIP2":
		content = bzip2.NewReader(scanned)
	}
	processed := &countingReader{r: content}
	out := &output{sink: sink, progress: req.RequestProgress, w: newWriter(req.Output, p, req.Input)}
	stats := func() Stats {
		return Stats{BytesScanned: scanned.n, BytesProcessed: processed.n, BytesReturned: out.returned}
	}
	out.stats = stats
	ev := &evaluator{p: p, out: out, ctx: ctx}
	if req.Input.CSV != nil {
		err = ev.csv(processed, req.Input.CSV)
	} else {
		err = ev.json(processed, strings.EqualFold(req.Input.JSON.Type, "DOCUMENT"))
	}
	if errors.Is(err, errLimit) {
		err = nil
	}
	if err == nil && p.q.aggregate {
		err = ev.emitAggregate()
	}
	if err == nil {
		err = out.flush()
	}
	return stats(), err
}

// errLimit stops the reading once LIMIT records were written.
var errLimit = errors.New("s3select: limit reached")

// evaluator runs a query over the records.
type evaluator struct {
	p       *prepared
	out     *output
	ctx     context.Context
	emitted int64
	seen    int64
}

// handle evaluates one record.
func (e *evaluator) handle(rec *record) error {
	e.seen++
	if e.seen%1024 == 0 {
		if err := e.ctx.Err(); err != nil {
			return err
		}
	}
	c := &evalCtx{rec: rec, alias: e.p.q.alias}
	if w := e.p.q.where; w != nil {
		v, err := w.eval(c)
		if err != nil {
			return err
		}
		if !v.truthy() {
			return nil
		}
	}
	q := e.p.q
	if q.aggregate {
		for _, a := range q.aggs {
			if err := a.accumulate(c); err != nil {
				return err
			}
		}
		return nil
	}
	if q.limit >= 0 && e.emitted >= q.limit {
		return errLimit
	}
	if err := e.out.record(c, q); err != nil {
		return err
	}
	e.emitted++
	if q.limit >= 0 && e.emitted >= q.limit {
		return errLimit
	}
	return nil
}

func (e *evaluator) emitAggregate() error {
	return e.out.record(&evalCtx{rec: &record{}}, e.p.q)
}

// csv reads the CSV records.
func (e *evaluator) csv(r io.Reader, in *CSVInput) error {
	if d := in.RecordDelimiter; len(d) == 1 && d != "\n" {
		r = &byteMapper{r: r, from: d[0], to: '\n'}
	}
	r = &lineLimiter{r: r, max: maxRecordSize}
	cr := csv.NewReader(bufio.NewReaderSize(r, 64<<10))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = false
	cr.LazyQuotes = true
	if in.FieldDelimiter != "" {
		cr.Comma, _ = utf8.DecodeRuneInString(in.FieldDelimiter)
	}
	if in.Comments != "" {
		cr.Comment, _ = utf8.DecodeRuneInString(in.Comments)
	}
	header := strings.ToUpper(in.FileHeaderInfo)
	var columns map[string]int
	first := true
	for {
		fields, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if isS3Error(err) {
				return err
			}
			return evalError("CSVParsingError", "the CSV content is invalid: %v", err)
		}
		if first {
			first = false
			switch header {
			case "USE":
				columns = make(map[string]int, len(fields))
				for i, f := range fields {
					if _, dup := columns[f]; !dup {
						columns[f] = i
					}
				}
				e.out.w.setColumns(fields)
				continue
			case "IGNORE":
				continue
			}
		}
		size := 0
		for _, f := range fields {
			size += len(f)
		}
		if size > maxRecordSize {
			return evalError("OverMaxRecordSize", "a record exceeds %d bytes", maxRecordSize)
		}
		if err := e.handle(&record{fields: fields, columns: columns}); err != nil {
			return err
		}
	}
}

// json reads the JSON records: every value of the content (and every
// element of a top-level array of a DOCUMENT), at the FROM path.
func (e *evaluator) json(r io.Reader, document bool) error {
	if !document {
		r = &lineLimiter{r: r, max: maxRecordSize}
	}
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))
	dec.UseNumber()
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if isS3Error(err) {
				return err
			}
			return evalError("JSONParsingError", "the JSON content is invalid: %v", err)
		}
		if len(raw) > maxRecordSize {
			return evalError("OverMaxRecordSize", "a record exceeds %d bytes", maxRecordSize)
		}
		raws := []json.RawMessage{raw}
		if document && bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			// The elements of a top-level array are the records.
			if err := json.Unmarshal(raw, &raws); err != nil {
				return evalError("JSONParsingError", "the JSON content is invalid: %v", err)
			}
		}
		for _, raw := range raws {
			var x any
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			if err := d.Decode(&x); err != nil {
				return evalError("JSONParsingError", "the JSON content is invalid: %v", err)
			}
			if len(e.p.q.from) > 0 {
				v := walk(x, e.p.q.from)
				switch v.k {
				case kMissing:
					continue
				case kDoc:
					if arr, ok := v.doc.([]any); ok {
						for _, el := range arr {
							if err := e.handle(&record{doc: el}); err != nil {
								return err
							}
						}
						continue
					}
				}
				if err := e.handle(&record{doc: v.toJSON()}); err != nil {
					return err
				}
				continue
			}
			if err := e.handle(&record{doc: x, raw: raw}); err != nil {
				return err
			}
		}
	}
}

// lineLimiter fails a stream holding a line longer than max bytes: the
// readers would otherwise buffer it whole.
type lineLimiter struct {
	r   io.Reader
	max int
	run int
}

func (l *lineLimiter) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	for _, b := range p[:n] {
		if b == '\n' {
			l.run = 0
			continue
		}
		if l.run++; l.run > l.max {
			return 0, evalError("OverMaxRecordSize", "a record exceeds %d bytes", l.max)
		}
	}
	return n, err
}

// byteMapper replaces a byte (a custom record delimiter) by another.
type byteMapper struct {
	r        io.Reader
	from, to byte
}

func (m *byteMapper) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	for i := range p[:n] {
		if p[i] == m.from {
			p[i] = m.to
		}
	}
	return n, err
}

// output buffers the serialized records for the sink.
type output struct {
	sink     Sink
	progress bool
	w        recordWriter
	buf      bytes.Buffer
	returned int64
	stats    func() Stats
}

func (o *output) record(c *evalCtx, q *query) error {
	if err := o.w.write(&o.buf, c, q); err != nil {
		return err
	}
	if o.buf.Len() >= outputChunk {
		return o.flush()
	}
	return nil
}

func (o *output) flush() error {
	if o.buf.Len() == 0 {
		return nil
	}
	o.returned += int64(o.buf.Len())
	if err := o.sink.Records(o.buf.Bytes()); err != nil {
		return err
	}
	o.buf.Reset()
	if o.progress {
		return o.sink.Progress(o.stats())
	}
	return nil
}

// recordWriter serializes the output records.
type recordWriter interface {
	setColumns(names []string)
	write(b *bytes.Buffer, c *evalCtx, q *query) error
}

func newWriter(out OutputSerialization, p *prepared, in InputSerialization) recordWriter {
	if c := out.CSV; c != nil {
		w := &csvWriter{delim: ",", recDelim: "\n", quote: `"`, escape: `"`, always: strings.EqualFold(c.QuoteFields, "ALWAYS")}
		if c.FieldDelimiter != "" {
			w.delim = c.FieldDelimiter
		}
		if c.RecordDelimiter != "" {
			w.recDelim = c.RecordDelimiter
		}
		if c.QuoteCharacter != "" {
			w.quote = c.QuoteCharacter
		}
		w.escape = w.quote
		if c.QuoteEscapeCharacter != "" {
			w.escape = c.QuoteEscapeCharacter
		}
		return w
	}
	w := &jsonWriter{recDelim: "\n", csvInput: in.CSV != nil}
	if d := out.JSON.RecordDelimiter; d != "" {
		w.recDelim = d
	}
	return w
}

// starValues returns the values of SELECT * for a record.
func starValues(rec *record) ([]string, []value) {
	if rec.doc == nil {
		vals := make([]value, len(rec.fields))
		for i, f := range rec.fields {
			vals[i] = stringValue(f)
		}
		return nil, vals
	}
	m, ok := rec.doc.(map[string]any)
	if !ok {
		return []string{"_1"}, []value{fromJSON(rec.doc)}
	}
	keys := orderedKeys(rec.raw, m)
	vals := make([]value, len(keys))
	for i, k := range keys {
		vals[i] = fromJSON(m[k])
	}
	return keys, vals
}

// orderedKeys returns the keys of a JSON object in their order in raw
// (sorted when raw is not available).
func orderedKeys(raw []byte, m map[string]any) []string {
	keys := make([]string, 0, len(m))
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if t, err := dec.Token(); err == nil && t == json.Delim('{') {
			for dec.More() {
				t, err := dec.Token()
				if err != nil {
					break
				}
				k, _ := t.(string)
				keys = append(keys, k)
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					break
				}
			}
			if len(keys) == len(m) {
				return keys
			}
		}
		keys = keys[:0]
	}
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// projection evaluates the items of a query (or SELECT *).
func projection(c *evalCtx, q *query) ([]string, []value, error) {
	if q.star {
		names, vals := starValues(c.rec)
		return names, vals, nil
	}
	names := make([]string, len(q.items))
	vals := make([]value, len(q.items))
	for i, it := range q.items {
		v, err := it.e.eval(c)
		if err != nil {
			return nil, nil, err
		}
		names[i], vals[i] = it.name, v
	}
	return names, vals, nil
}

type csvWriter struct {
	delim, recDelim, quote, escape string
	always                         bool
}

func (w *csvWriter) setColumns([]string) {}

func (w *csvWriter) write(b *bytes.Buffer, c *evalCtx, q *query) error {
	_, vals, err := projection(c, q)
	if err != nil {
		return err
	}
	for i, v := range vals {
		if i > 0 {
			b.WriteString(w.delim)
		}
		s := v.text()
		if w.always || strings.Contains(s, w.delim) || strings.Contains(s, w.quote) || strings.ContainsAny(s, "\r\n") || strings.Contains(s, w.recDelim) {
			b.WriteString(w.quote)
			b.WriteString(strings.ReplaceAll(s, w.quote, w.escape+w.quote))
			b.WriteString(w.quote)
		} else {
			b.WriteString(s)
		}
	}
	b.WriteString(w.recDelim)
	return nil
}

type jsonWriter struct {
	recDelim string
	csvInput bool
	columns  []string
}

func (w *jsonWriter) setColumns(names []string) { w.columns = names }

func (w *jsonWriter) write(b *bytes.Buffer, c *evalCtx, q *query) error {
	if q.star && c.rec.doc != nil && c.rec.raw != nil {
		if err := json.Compact(b, c.rec.raw); err != nil {
			return evalError("JSONParsingError", "the JSON content is invalid: %v", err)
		}
		b.WriteString(w.recDelim)
		return nil
	}
	names, vals, err := projection(c, q)
	if err != nil {
		return err
	}
	b.WriteByte('{')
	first := true
	for i, v := range vals {
		if v.k == kMissing {
			continue
		}
		name := ""
		if names != nil {
			name = names[i]
		}
		if q.star && w.csvInput {
			name = "_" + strconv.Itoa(i+1)
			if i < len(w.columns) {
				name = w.columns[i]
			}
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, err := json.Marshal(name)
		if err != nil {
			return err
		}
		b.Write(k)
		b.WriteByte(':')
		data, err := json.Marshal(v.toJSON())
		if err != nil {
			return fmt.Errorf("s3select: encode a value: %w", err)
		}
		b.Write(data)
	}
	b.WriteByte('}')
	b.WriteString(w.recDelim)
	return nil
}
