package storage

import (
	"bytes"
	"encoding/binary"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// updateFixtures rewrites the binary xl.meta fixtures of testdata/xlmeta
// (go test ./internal/service/storage -run TestXLMetaFixtures -update).
var updateFixtures = flag.Bool("update", false, "rewrite the xl.meta golden fixtures")

const (
	dd1 = "3f2b2b48-5c3a-4a1e-8a2a-123456789abc"
	dd2 = "9d4e0c3a-1111-4222-8333-444455556666"
)

var t0 = time.Date(2026, 10, 8, 10, 0, 0, 123456789, time.UTC)

// sampleFileInfos covers the shapes of version persisted by the object
// layer: inline and data-dir objects, erasure layouts, delete markers.
func sampleFileInfos() map[string]FileInfo {
	return map[string]FileInfo{
		"inline null version": {
			ModTime: t0, Size: 5, Data: []byte("hello"),
			Metadata: map[string]string{"content-type": "text/plain", "etag": "5d41402abc4b2a76b9719d911017c592"},
		},
		"empty inline object": {VersionID: "v-empty", ModTime: t0.Add(time.Minute), Data: []byte{}, Versioned: true},
		"data dir with parts and erasure": {
			VersionID: "8a3c1c52-0000-4000-8000-000000000001", ModTime: t0.Add(time.Hour), Size: 11, DataDir: dd1, Mode: 0o644,
			Metadata: map[string]string{
				"etag": "abc-2", "content-type": "application/octet-stream", "x-amz-tagging": "a=1&b=2",
				"x-amz-meta-owner": "me", "x-amz-meta-": "prefix only", "cache-control": "no-cache", "x-amz-meta-empty": "",
				"expires": "",
			},
			Parts: []PartInfo{
				{Number: 1, ETag: "e1", Size: 5, ActualSize: 5, ModTime: t0, Index: []byte{1, 2}, Checksums: map[string]string{"CRC32": "AAAA"}},
				{Number: 2, ETag: "e2", Size: 6, ActualSize: 6},
			},
			Erasure: ErasureInfo{Algorithm: "rs-vandermonde", DataBlocks: 2, ParityBlocks: 2, BlockSize: 1 << 20, Index: 3,
				Distribution: []int{3, 4, 1, 2}, Checksums: []ChecksumInfo{{PartNumber: 1, Algorithm: "highwayhash256S", Hash: []byte{9, 9}}}},
			Checksum:         []byte{0xca, 0xfe},
			Versioned:        true,
			ReplicationState: map[string]string{"arn:target": "PENDING"},
			TransitionStatus: "complete",
		},
		"delete marker": {VersionID: "dm-1", ModTime: t0.Add(2 * time.Hour), Deleted: true, Versioned: true},
		"zero time":     {VersionID: "z", Size: 0, Metadata: map[string]string{}},
	}
}

// normalized is the FileInfo a decoded version is expected to equal.
func normalized(fi FileInfo, withData bool) FileInfo {
	if fi.Metadata == nil {
		fi.Metadata = map[string]string{}
	}
	if !withData {
		fi.Data = nil
	}
	fi.ModTime = fi.ModTime.UTC()
	return fi
}

func TestXLMetaRoundTrip(t *testing.T) {
	for name, fi := range sampleFileInfos() {
		t.Run(name, func(t *testing.T) {
			m := &xlMeta{}
			_, err := m.AddVersion(versionFromFileInfo(fi))
			require.NoError(t, err)
			raw, err := m.encode()
			require.NoError(t, err)
			require.Equal(t, xlMetaMagic, string(raw[:4]))

			full, err := decodeXLMeta(raw, true)
			require.NoError(t, err)
			require.Len(t, full.Versions, 1)
			got := full.Versions[0].fileInfo("b", "k")
			want := normalized(fi, true)
			want.Volume, want.Name = "b", "k"
			assert.Equal(t, want, got)

			// The metadata section alone decodes without the content.
			end, err := xlMetaSectionEnd(raw)
			require.NoError(t, err)
			metaOnly, err := decodeXLMeta(raw[:end], false)
			require.NoError(t, err)
			want.Data = nil
			assert.Equal(t, want, metaOnly.Versions[0].fileInfo("b", "k"))
			infos, err := DecodeFileInfos("b", "k", raw[:end])
			require.NoError(t, err)
			want.IsLatest, want.NumVersions = true, 1
			assert.Equal(t, want, infos[0])

			// Encoding is deterministic.
			again, err := full.encode()
			require.NoError(t, err)
			assert.Equal(t, raw, again)
		})
	}
}

func TestXLMetaMultipleVersions(t *testing.T) {
	m := &xlMeta{}
	for _, fi := range sampleFileInfos() {
		_, err := m.AddVersion(versionFromFileInfo(fi))
		require.NoError(t, err)
	}
	raw, err := m.encode()
	require.NoError(t, err)
	got, err := decodeXLMeta(raw, true)
	require.NoError(t, err)
	require.Len(t, got.Versions, 5)
	assert.Equal(t, []string{"dm-1", "8a3c1c52-0000-4000-8000-000000000001", "v-empty", "", "z"}, versionIDs(got))
	assert.Equal(t, "hello", string(got.Versions[3].Data))
	assert.NotNil(t, got.Versions[2].Data, "an empty inline content stays inline")
	assert.Nil(t, got.Versions[1].Data)

	// A document without inline content has no data section.
	plain := &xlMeta{}
	_, err = plain.AddVersion(xlVersion{ID: "a", Type: xlTypeObject, ModTime: 1, DataDir: dd1})
	require.NoError(t, err)
	raw, err = plain.encode()
	require.NoError(t, err)
	end, err := xlMetaSectionEnd(raw)
	require.NoError(t, err)
	assert.Equal(t, len(raw), end)
}

func TestXLMetaOperations(t *testing.T) {
	m := &xlMeta{}
	add := func(v xlVersion) *xlVersion {
		t.Helper()
		replaced, err := m.AddVersion(v)
		require.NoError(t, err)
		return replaced
	}
	assert.Nil(t, add(xlVersion{ID: "a", Type: xlTypeObject, ModTime: 10}))
	assert.Nil(t, add(xlVersion{ID: "", Type: xlTypeObject, ModTime: 20, DataDir: dd1}))
	assert.Nil(t, add(xlVersion{ID: "b", Type: xlTypeObject, ModTime: 5}))
	assert.Equal(t, []string{"", "a", "b"}, versionIDs(m))

	// The same modification time: the last write sorts first.
	assert.Nil(t, add(xlVersion{ID: "c", Type: xlTypeObject, ModTime: 10}))
	assert.Equal(t, []string{"", "c", "a", "b"}, versionIDs(m))

	// "null" designates the null version, which is replaced, not duplicated.
	replaced := add(xlVersion{ID: "null", Type: xlTypeObject, ModTime: 1, Size: 9})
	require.NotNil(t, replaced)
	assert.Equal(t, dd1, replaced.DataDir)
	assert.Equal(t, []string{"c", "a", "b", ""}, versionIDs(m))

	// Delete markers never carry content.
	add(xlVersion{ID: "dm", Type: xlTypeDeleteMarker, ModTime: 30, Size: 4, DataDir: dd2, Data: []byte("x"), Flags: xlFlagInlineData})
	latest, ok := m.Latest()
	require.True(t, ok)
	assert.Equal(t, xlVersion{ID: "dm", Type: xlTypeDeleteMarker, ModTime: 30}, latest)

	for _, id := range []string{"", "null"} {
		i, err := m.FindVersion(id)
		require.NoError(t, err)
		assert.Equal(t, 4, i)
	}
	_, err := m.FindVersion("zz")
	assert.ErrorIs(t, err, domain.ErrNoSuchVersion)

	listed := m.ListVersions()
	listed[0].ID = "changed"
	assert.Equal(t, "dm", m.Versions[0].ID, "ListVersions returns a copy")

	removed, err := m.DeleteVersion("null")
	require.NoError(t, err)
	assert.Equal(t, int64(9), removed.Size)
	_, err = m.DeleteVersion("null")
	assert.ErrorIs(t, err, domain.ErrNoSuchVersion)
	assert.Equal(t, []string{"dm", "c", "a", "b"}, versionIDs(m))

	assert.True(t, (&xlMeta{Versions: []xlVersion{{DataDir: dd1}}}).sharesDataDir(dd1))
	assert.False(t, m.sharesDataDir(dd1))
	assert.False(t, m.sharesDataDir(""))
	_, ok = (&xlMeta{}).Latest()
	assert.False(t, ok)
}

func TestXLMetaAddVersionValidation(t *testing.T) {
	tests := []struct {
		name string
		v    xlVersion
	}{
		{name: "unknown type", v: xlVersion{ID: "a"}},
		{name: "negative size", v: xlVersion{ID: "a", Type: xlTypeObject, Size: -1}},
		{name: "data dir traversal", v: xlVersion{ID: "a", Type: xlTypeObject, DataDir: "../../etc"}},
		{name: "data dir not a uuid", v: xlVersion{ID: "a", Type: xlTypeObject, DataDir: "data"}},
		{name: "part number zero", v: xlVersion{ID: "a", Type: xlTypeObject, Parts: []xlPart{{Number: 0, Size: 1}}}},
		{name: "negative part size", v: xlVersion{ID: "a", Type: xlTypeObject, Parts: []xlPart{{Number: 1, Size: -1}}}},
		{name: "negative erasure", v: xlVersion{ID: "a", Type: xlTypeObject, Erasure: &xlErasure{Data: -1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &xlMeta{}
			_, err := m.AddVersion(tt.v)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			assert.Empty(t, m.Versions)
		})
	}
}

func TestXLMetaCompact(t *testing.T) {
	m := &xlMeta{Versions: []xlVersion{
		{ID: "a", Type: xlTypeObject, ModTime: 1, Meta: map[string]string{}, UserMetadata: map[string]string{}, Replication: map[string]string{},
			Parts: []xlPart{{Number: 1, Checksums: map[string]string{}}}},
		{ID: "null", Type: xlTypeObject, ModTime: 3},
		{ID: "a", Type: xlTypeObject, ModTime: 2},
		{ID: "dm", Type: xlTypeDeleteMarker, ModTime: 4, DataDir: dd1, Size: 3, Flags: xlFlagInlineData | xlFlagVersioned},
	}}
	m.Compact()
	assert.Equal(t, []string{"dm", "", "a"}, versionIDs(m))
	assert.Equal(t, int64(2), m.Versions[2].ModTime, "the most recent duplicate wins")
	assert.Equal(t, xlVersion{ID: "dm", Type: xlTypeDeleteMarker, ModTime: 4, Flags: xlFlagVersioned}, m.Versions[0])

	m2 := &xlMeta{Versions: []xlVersion{{ID: "a", Type: xlTypeObject, ModTime: 1, Meta: map[string]string{},
		Parts: []xlPart{{Number: 1, Checksums: map[string]string{}}}}}}
	m2.Compact()
	assert.Nil(t, m2.Versions[0].Meta)
	assert.Nil(t, m2.Versions[0].Parts[0].Checksums)
}

func TestXLMetaSignature(t *testing.T) {
	v := versionFromFileInfo(sampleFileInfos()["data dir with parts and erasure"])
	sig := v.signature()
	assert.NotEqual(t, [4]byte{}, sig)
	other := v
	other.Erasure = &xlErasure{Index: 1}
	assert.Equal(t, sig, other.signature(), "the per-drive erasure index is not part of the signature")
	other.ModTime++
	assert.NotEqual(t, sig, other.signature())
}

// encodeRaw builds a binary xl.meta around an arbitrary metadata payload.
func encodeRaw(major, minor uint16, payload []byte, dataSection []byte) []byte {
	out := make([]byte, xlMetaHeaderSize)
	copy(out, xlMetaMagic)
	binary.BigEndian.PutUint16(out[4:], major)
	binary.BigEndian.PutUint16(out[6:], minor)
	binary.BigEndian.PutUint32(out[8:], uint32(len(payload)))
	out = append(out, payload...)
	out = binary.BigEndian.AppendUint32(out, crc32c(out))
	if dataSection != nil {
		out = append(out, dataSection...)
		out = binary.BigEndian.AppendUint32(out, crc32c(dataSection))
	}
	return out
}

func mustMsgpack(t *testing.T, v any) []byte {
	t.Helper()
	b, err := msgpack.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestXLMetaForwardCompatibility(t *testing.T) {
	// A newer minor version with unknown fields is readable.
	payload := mustMsgpack(t, map[string]any{
		"vs":     []map[string]any{{"id": "a", "ty": 1, "mt": 42, "sz": 3, "future": []any{1, "x", map[string]any{"k": true}}}},
		"future": "ignored",
	})
	m, err := decodeXLMeta(encodeRaw(xlMetaMajor, xlMetaMinor+5, payload, nil), true)
	require.NoError(t, err)
	require.Len(t, m.Versions, 1)
	assert.Equal(t, int64(3), m.Versions[0].Size)

	// A newer major version is refused.
	_, err = decodeXLMeta(encodeRaw(xlMetaMajor+1, 0, payload, nil), true)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}

func TestXLMetaDecodeErrors(t *testing.T) {
	m := &xlMeta{}
	_, err := m.AddVersion(versionFromFileInfo(FileInfo{VersionID: "a", ModTime: t0, Size: 2, Data: []byte("hi")}))
	require.NoError(t, err)
	good, err := m.encode()
	require.NoError(t, err)
	end, err := xlMetaSectionEnd(good)
	require.NoError(t, err)

	flip := func(i int) []byte {
		b := bytes.Clone(good)
		b[i] ^= 0xff
		return b
	}
	badVersion := mustMsgpack(t, map[string]any{"vs": []map[string]any{{"id": "a", "ty": 1, "dd": "../x"}}})
	wrongTypes := mustMsgpack(t, map[string]any{"vs": "not an array"})
	inlineMissing := mustMsgpack(t, map[string]any{"vs": []map[string]any{{"id": "a", "ty": 1, "fl": xlFlagInlineData}}})

	tests := []struct {
		name    string
		raw     []byte
		corrupt bool
	}{
		{name: "empty", raw: nil},
		{name: "garbage", raw: []byte("definitely not xl.meta")},
		{name: "bad magic", raw: append([]byte("XXXX"), good[4:]...)},
		{name: "truncated header", raw: good[:10]},
		{name: "section beyond the end", raw: good[:end-1], corrupt: true},
		{name: "metadata checksum", raw: flip(xlMetaHeaderSize + 2), corrupt: true},
		{name: "data checksum", raw: flip(len(good) - 1), corrupt: true},
		{name: "data section truncated", raw: good[:end+2], corrupt: true},
		{name: "invalid msgpack", raw: encodeRaw(xlMetaMajor, xlMetaMinor, []byte{0xc1}, nil)},
		{name: "huge declared array", raw: encodeRaw(xlMetaMajor, xlMetaMinor, []byte{0x81, 0xa2, 'v', 's', 0xdd, 0xff, 0xff, 0xff, 0xff}, nil)},
		{name: "wrong field types", raw: encodeRaw(xlMetaMajor, xlMetaMinor, wrongTypes, nil)},
		{name: "unsafe data dir", raw: encodeRaw(xlMetaMajor, xlMetaMinor, badVersion, nil)},
		{name: "inline content missing", raw: encodeRaw(xlMetaMajor, xlMetaMinor, inlineMissing, mustMsgpack(t, map[string][]byte{"b": {1}})), corrupt: true},
		{name: "invalid data section", raw: encodeRaw(xlMetaMajor, xlMetaMinor, inlineMissing, []byte{0xc1})},
		{name: "legacy invalid json", raw: []byte(`{"version":`)},
		{name: "legacy unknown version", raw: []byte(`{"version":7,"versions":[]}`)},
		{name: "legacy unsafe data dir", raw: []byte(`{"version":1,"versions":[{"id":"a","dataDir":"../../x","modTime":"2026-01-01T00:00:00Z","size":1}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeXLMeta(tt.raw, true)
			assert.ErrorIs(t, err, domain.ErrInvalidFormat)
			if tt.corrupt {
				assert.ErrorIs(t, err, ErrFileCorrupt)
				assert.ErrorIs(t, err, domain.ErrFaultyDisk)
			}
		})
	}
	// Without data, a damaged data section does not matter.
	_, err = decodeXLMeta(flip(len(good)-1), false)
	assert.NoError(t, err)
}

func TestXLMetaLegacyJSON(t *testing.T) {
	raw := []byte(`  {"version":1,"versions":[
		{"id":"v1","modTime":"2026-10-08T10:00:00Z","size":5,"data":"aGVsbG8=","versioned":true,
		 "metadata":{"content-type":"text/plain","etag":"x","x-amz-meta-a":"1"}},
		{"id":"dm","deleted":true,"modTime":"2026-10-08T11:00:00Z","size":0},
		{"dataDir":"` + dd1 + `","modTime":"2026-10-08T09:00:00Z","size":7,"parts":[{"Number":1,"Size":7}],"erasure":{"DataBlocks":1,"Index":1}}]}`)
	m, err := decodeXLMeta(raw, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"dm", "v1", ""}, versionIDs(m))
	v1 := m.Versions[1].fileInfo("b", "k")
	assert.Equal(t, "hello", string(v1.Data))
	assert.Equal(t, map[string]string{"content-type": "text/plain", "etag": "x", "x-amz-meta-a": "1"}, v1.Metadata)
	assert.True(t, v1.Versioned)
	assert.True(t, m.Versions[0].fileInfo("b", "k").Deleted)
	assert.Equal(t, 1, m.Versions[2].fileInfo("b", "k").Erasure.DataBlocks)

	noData, err := decodeXLMeta(raw, false)
	require.NoError(t, err)
	assert.Nil(t, noData.Versions[1].Data)

	// The next write converts the document to the binary format.
	out, err := m.encode()
	require.NoError(t, err)
	assert.Equal(t, xlMetaMagic, string(out[:4]))
}

// TestXLMetaFixtures decodes the on-disk documents of every supported
// format version and checks that the current encoder reproduces the
// binary fixture byte for byte.
func TestXLMetaFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "xlmeta")
	fixture := &xlMeta{}
	for _, fi := range sampleFileInfos() {
		_, err := fixture.AddVersion(versionFromFileInfo(fi))
		require.NoError(t, err)
	}
	encoded, err := fixture.encode()
	require.NoError(t, err)
	v1 := filepath.Join(dir, "v1.0.meta")
	if *updateFixtures {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(v1, encoded, 0o600))
	}

	raw, err := os.ReadFile(v1)
	require.NoError(t, err)
	assert.Equal(t, encoded, raw, "the v1.0 encoding changed: bump the minor/major version or run with -update")
	m, err := decodeXLMeta(raw, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"dm-1", "8a3c1c52-0000-4000-8000-000000000001", "v-empty", "", "z"}, versionIDs(m))

	legacy, err := os.ReadFile(filepath.Join(dir, "legacy-json.meta"))
	require.NoError(t, err)
	lm, err := decodeXLMeta(legacy, true)
	require.NoError(t, err)
	require.NotEmpty(t, lm.Versions)
	assert.Equal(t, "hello", string(lm.Versions[len(lm.Versions)-1].Data))
}

func TestCheckMsgpack(t *testing.T) {
	valid := mustMsgpack(t, map[string]any{
		"s": "str", "mid": string(bytes.Repeat([]byte("m"), 100)), "bin16": make([]byte, 300), "long": string(bytes.Repeat([]byte("x"), 300)), "huge": string(bytes.Repeat([]byte("y"), 70000)),
		"b": []byte{1}, "i8": int8(-100), "i16": int16(-30000), "i32": int32(-2000000000), "i64": int64(-1 << 40),
		"u8": uint8(200), "u16": uint16(60000), "u32": uint32(4000000000), "u64": uint64(1 << 40),
		"f32": float32(1.5), "f64": 2.5, "nil": nil, "t": true, "f": false, "arr": make([]int, 20),
		"time": time.Unix(1, 2), "neg": -1,
	})
	require.NoError(t, checkMsgpack(valid, 8))
	bigMap := make(map[string]int, 20)
	for i := range 20 {
		bigMap[string(rune('a'+i))] = i
	}
	require.NoError(t, checkMsgpack(mustMsgpack(t, bigMap), 8))
	require.NoError(t, checkMsgpack(mustMsgpack(t, make([]int, 70000)), 8))
	require.NoError(t, checkMsgpack(mustMsgpack(t, map[string]any{"bin": make([]byte, 70000)}), 8))

	deep := []byte{}
	for range 10 {
		deep = append(deep, 0x91) // fixarray of one element
	}
	deep = append(deep, 0x01)

	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{name: "empty", raw: nil, want: errMsgpackTruncated},
		{name: "trailing", raw: []byte{0x01, 0x02}, want: errMsgpackTrailing},
		{name: "never used code", raw: []byte{0xc1}, want: errMsgpackCode},
		{name: "truncated string", raw: []byte{0xa5, 'a'}, want: errMsgpackTruncated},
		{name: "truncated length", raw: []byte{0xda, 0x01}, want: errMsgpackTruncated},
		{name: "truncated ext", raw: []byte{0xc7, 0x05, 0x01}, want: errMsgpackTruncated},
		{name: "ext16 length", raw: []byte{0xc8, 0x00}, want: errMsgpackTruncated},
		{name: "ext32 length", raw: []byte{0xc9, 0x00, 0x00, 0x00, 0x08, 0x01}, want: errMsgpackTruncated},
		{name: "huge array", raw: []byte{0xdd, 0xff, 0xff, 0xff, 0xff, 0x01}, want: errMsgpackTooLarge},
		{name: "huge map", raw: []byte{0xdf, 0x7f, 0xff, 0xff, 0xff}, want: errMsgpackTooLarge},
		{name: "map16 too large", raw: []byte{0xde, 0x00, 0x02, 0x01, 0x01}, want: errMsgpackTooLarge},
		{name: "array16 truncated element", raw: []byte{0xdc, 0x00, 0x01, 0xcd, 0x01}, want: errMsgpackTruncated},
		{name: "fixmap missing value", raw: []byte{0x81, 0x01}, want: errMsgpackTooLarge},
		{name: "too deep", raw: deep, want: errMsgpackTooDeep},
		{name: "fixext truncated", raw: []byte{0xd8, 0x01, 0x02}, want: errMsgpackTruncated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, checkMsgpack(tt.raw, 8), tt.want)
		})
	}
	assert.NoError(t, checkMsgpack(deep, 16))
}

// FuzzDecodeXLMeta checks that no input makes the decoder panic or
// allocate without bounds, and that whatever decodes re-encodes to a
// document that decodes to the same versions.
func FuzzDecodeXLMeta(f *testing.F) {
	m := &xlMeta{}
	for _, fi := range sampleFileInfos() {
		if _, err := m.AddVersion(versionFromFileInfo(fi)); err != nil {
			f.Fatal(err)
		}
	}
	raw, err := m.encode()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	end, _ := xlMetaSectionEnd(raw)
	f.Add(raw[:end])
	f.Add([]byte(`{"version":1,"versions":[{"id":"a","modTime":"2026-10-08T10:00:00Z","size":1,"data":"eA=="}]}`))
	f.Add([]byte{})
	f.Add([]byte(xlMetaMagic + "\x00\x01\x00\x00\x00\x00\x00\x01\xc0"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, withData := range []bool{true, false} {
			m, err := decodeXLMeta(data, withData)
			if err != nil {
				continue
			}
			out, err := m.encode()
			if err != nil {
				continue // e.g. a legacy document that cannot be expressed
			}
			again, err := decodeXLMeta(out, withData)
			if err != nil {
				t.Fatalf("re-encoded document does not decode: %v", err)
			}
			if len(again.Versions) != len(m.Versions) {
				t.Fatalf("re-encoded document has %d versions, want %d", len(again.Versions), len(m.Versions))
			}
		}
	})
}

func versionIDs(m *xlMeta) []string {
	out := make([]string, 0, len(m.Versions))
	for _, v := range m.Versions {
		out = append(out, v.ID)
	}
	return out
}
