package object

import (
	"encoding/base64"
	"hash/crc32"
	"hash/crc64"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// crcOf returns the base64 S3 checksum of data.
func crcOf(t *testing.T, algo domain.ChecksumAlgorithm, data []byte) string {
	t.Helper()
	h, err := newChecksumHash(algo)
	require.NoError(t, err)
	_, _ = h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestCRCCombine(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, algo := range []domain.ChecksumAlgorithm{domain.ChecksumCRC32, domain.ChecksumCRC32C, domain.ChecksumCRC64NVME} {
		t.Run(string(algo), func(t *testing.T) {
			for _, sizes := range [][]int{{1}, {0, 5}, {5, 0}, {1, 1}, {7, 1000, 3}, {4096, 4096, 17}, {0, 0, 0}, {70000, 1}} {
				var whole []byte
				var parts []partSum
				for _, n := range sizes {
					p := make([]byte, n)
					for i := range p {
						p[i] = byte(rng.IntN(256))
					}
					whole = append(whole, p...)
					parts = append(parts, partSum{sum: domain.Checksum{Algorithm: algo, Value: crcOf(t, algo, p)}, size: int64(n)})
				}
				got, ok, err := fullObjectChecksum(algo, parts)
				require.NoError(t, err)
				require.True(t, ok)
				assert.Equal(t, crcOf(t, algo, whole), got.Value, "sizes %v", sizes)
				assert.Equal(t, domain.ChecksumTypeFullObject, got.Type)
			}
		})
	}
	_, err := newCRCCombiner(domain.ChecksumSHA256)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestCRC64NVMEVector checks the CRC-64/NVME parameters against the
// standard check value of "123456789".
func TestCRC64NVMEVector(t *testing.T) {
	h := crc64.New(crc64.MakeTable(crc64NVMEPoly))
	_, _ = h.Write([]byte("123456789"))
	assert.Equal(t, uint64(0xae8b14860a799888), h.Sum64())
	assert.Equal(t, uint32(0xcbf43926), crc32.ChecksumIEEE([]byte("123456789")))
}

func TestMultipartChecksumTypes(t *testing.T) {
	p1, p2 := []byte("first part"), []byte("second")
	whole := append(append([]byte{}, p1...), p2...)
	sums := func(algo domain.ChecksumAlgorithm) []partSum {
		return []partSum{
			{sum: domain.Checksum{Algorithm: algo, Value: crcOf(t, algo, p1)}, size: int64(len(p1))},
			{sum: domain.Checksum{Algorithm: algo, Value: crcOf(t, algo, p2)}, size: int64(len(p2))},
		}
	}
	full, err := multipartChecksum(domain.ChecksumCRC64NVME, domain.ChecksumTypeFullObject, sums(domain.ChecksumCRC64NVME), domain.Checksum{})
	require.NoError(t, err)
	assert.Equal(t, domain.Checksum{Algorithm: domain.ChecksumCRC64NVME, Value: crcOf(t, domain.ChecksumCRC64NVME, whole), Type: domain.ChecksumTypeFullObject}, full)

	declared := domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crcOf(t, domain.ChecksumCRC32C, whole)}
	full, err = multipartChecksum(domain.ChecksumCRC32C, domain.ChecksumTypeFullObject, sums(domain.ChecksumCRC32C), declared)
	require.NoError(t, err)
	assert.Equal(t, declared.Value, full.Value)

	_, err = multipartChecksum(domain.ChecksumCRC32C, domain.ChecksumTypeFullObject, sums(domain.ChecksumCRC32C),
		domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crcOf(t, domain.ChecksumCRC32C, p1)})
	assert.ErrorIs(t, err, domain.ErrBadDigest)

	comp, err := multipartChecksum(domain.ChecksumSHA256, domain.ChecksumTypeComposite, sums(domain.ChecksumSHA256), domain.Checksum{})
	require.NoError(t, err)
	assert.Equal(t, domain.ChecksumTypeComposite, comp.Type)
	assert.Regexp(t, `-2$`, comp.Value)
}

func TestUploadChecksumType(t *testing.T) {
	tests := []struct {
		algo domain.ChecksumAlgorithm
		typ  domain.ChecksumType
		want domain.ChecksumType
		ok   bool
	}{
		{domain.ChecksumNone, domain.ChecksumTypeNone, domain.ChecksumTypeNone, true},
		{domain.ChecksumNone, domain.ChecksumTypeFullObject, "", false},
		{domain.ChecksumCRC32, domain.ChecksumTypeNone, domain.ChecksumTypeComposite, true},
		{domain.ChecksumCRC32, domain.ChecksumTypeFullObject, domain.ChecksumTypeFullObject, true},
		{domain.ChecksumCRC32C, domain.ChecksumTypeComposite, domain.ChecksumTypeComposite, true},
		{domain.ChecksumCRC64NVME, domain.ChecksumTypeNone, domain.ChecksumTypeFullObject, true},
		{domain.ChecksumCRC64NVME, domain.ChecksumTypeComposite, "", false},
		{domain.ChecksumSHA1, domain.ChecksumTypeFullObject, "", false},
		{domain.ChecksumSHA256, domain.ChecksumTypeNone, domain.ChecksumTypeComposite, true},
	}
	for _, tt := range tests {
		got, err := uploadChecksumType(tt.algo, tt.typ)
		if !tt.ok {
			assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%s %s", tt.algo, tt.typ)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, "%s %s", tt.algo, tt.typ)
	}
	assert.Equal(t, domain.ChecksumTypeComposite, checksumTypeOf("abc=-3", ""))
	assert.Equal(t, domain.ChecksumTypeFullObject, checksumTypeOf("abc=", ""))
	assert.Equal(t, domain.ChecksumTypeFullObject, checksumTypeOf("abc=-3", "FULL_OBJECT"))
}
