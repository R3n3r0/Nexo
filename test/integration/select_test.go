package integration_test

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectRecords runs SelectObjectContent and returns the records and the
// stats of the stream.
func selectRecords(t *testing.T, e *env, in *s3.SelectObjectContentInput) (string, *types.Stats, error) {
	t.Helper()
	out, err := e.root.SelectObjectContent(e.ctx, in)
	if err != nil {
		return "", nil, err
	}
	stream := out.GetStream()
	defer func() { _ = stream.Close() }()
	var records strings.Builder
	var stats *types.Stats
	ended := false
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.SelectObjectContentEventStreamMemberRecords:
			records.Write(v.Value.Payload)
		case *types.SelectObjectContentEventStreamMemberStats:
			stats = v.Value.Details
		case *types.SelectObjectContentEventStreamMemberEnd:
			ended = true
		}
	}
	if err := stream.Err(); err != nil {
		return records.String(), stats, err
	}
	assert.True(t, ended, "the stream ends with an End event")
	return records.String(), stats, nil
}

// TestSelectObjectContent (S-OBJ-16): SQL over CSV and JSON objects
// through the S3 API, the event stream decoded by the AWS SDK.
//
// covers: S-OBJ-16
func TestSelectObjectContent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "select"
	e.mkBucket(bucket)
	csvData := "name,age,city\nAlice,30,Rome\nBob,25,Milan\nCarol,41,Rome\n"
	_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("people.csv"), Body: strings.NewReader(csvData)})
	require.NoError(t, err)

	csvIn := &types.InputSerialization{CSV: &types.CSVInput{FileHeaderInfo: types.FileHeaderInfoUse}}
	records, stats, err := selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("people.csv"), ExpressionType: types.ExpressionTypeSql,
		Expression:          aws.String("SELECT s.name, s.age FROM S3Object s WHERE s.city = 'Rome' AND CAST(s.age AS INT) > 18"),
		InputSerialization:  csvIn,
		OutputSerialization: &types.OutputSerialization{CSV: &types.CSVOutput{}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Alice,30\nCarol,41\n", records)
	require.NotNil(t, stats)
	assert.Equal(t, int64(len(csvData)), aws.ToInt64(stats.BytesScanned))
	assert.Equal(t, int64(len(records)), aws.ToInt64(stats.BytesReturned))

	records, _, err = selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("people.csv"), ExpressionType: types.ExpressionTypeSql,
		Expression:          aws.String("SELECT COUNT(*), AVG(age) FROM S3Object"),
		InputSerialization:  csvIn,
		OutputSerialization: &types.OutputSerialization{JSON: &types.JSONOutput{}},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"_1":3,"_2":32}`+"\n", records)

	// JSON lines, compressed with GZIP.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, err = zw.Write([]byte(`{"id":1,"tags":["x"]}` + "\n" + `{"id":2,"tags":["y","z"]}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("events.json.gz"), Body: bytes.NewReader(gz.Bytes())})
	require.NoError(t, err)
	records, _, err = selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("events.json.gz"), ExpressionType: types.ExpressionTypeSql,
		Expression:          aws.String("SELECT s.id, s.tags[1] AS second FROM S3Object[*] s WHERE s.id >= 2"),
		InputSerialization:  &types.InputSerialization{JSON: &types.JSONInput{Type: types.JSONTypeLines}, CompressionType: types.CompressionTypeGzip},
		OutputSerialization: &types.OutputSerialization{JSON: &types.JSONOutput{}},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"id":2,"second":"z"}`+"\n", records)

	// An invalid query is refused before the stream; an evaluation error
	// ends the stream with an error event.
	_, _, err = selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("people.csv"), ExpressionType: types.ExpressionTypeSql,
		Expression: aws.String("SELECT FROM S3Object"), InputSerialization: csvIn,
		OutputSerialization: &types.OutputSerialization{CSV: &types.CSVOutput{}},
	})
	assert.Equal(t, "ParseUnexpectedToken", apiCode(err))
	_, _, err = selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("people.csv"), ExpressionType: types.ExpressionTypeSql,
		Expression: aws.String("SELECT CAST(name AS INT) FROM S3Object"), InputSerialization: csvIn,
		OutputSerialization: &types.OutputSerialization{CSV: &types.CSVOutput{}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CastFailed")
	_, _, err = selectRecords(t, e, &s3.SelectObjectContentInput{
		Bucket: aws.String(bucket), Key: aws.String("missing.csv"), ExpressionType: types.ExpressionTypeSql,
		Expression: aws.String("SELECT * FROM S3Object"), InputSerialization: csvIn,
		OutputSerialization: &types.OutputSerialization{CSV: &types.CSVOutput{}},
	})
	assert.Equal(t, "NoSuchKey", apiCode(err))
}
