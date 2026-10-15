package object

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/r3n3r0/nexo/internal/domain"
)

// crc64NVMEPoly is the reflected polynomial of CRC-64/NVME, the
// CRC64NVME additional checksum (hash/crc64 takes the reflected form).
const crc64NVMEPoly = 0x9a6c9329ac4bc9b5

// crcCombiner combines the CRCs of consecutive byte ranges into the CRC
// of their concatenation (zlib's crc32_combine, generalized to the
// reflected CRCs of 32 and 64 bits with the usual pre- and
// post-conditioning): the FULL_OBJECT checksum of a multipart upload is
// computed from the CRCs of its parts without reading them.
type crcCombiner struct {
	// poly is the reflected polynomial, width its degree (32 or 64).
	poly  uint64
	width int
	// size is the length in bytes of an encoded CRC.
	size int
}

// newCRCCombiner returns the combiner of a CRC algorithm.
func newCRCCombiner(algo domain.ChecksumAlgorithm) (crcCombiner, error) {
	switch algo {
	case domain.ChecksumCRC32:
		return crcCombiner{poly: crc32.IEEE, width: 32, size: 4}, nil
	case domain.ChecksumCRC32C:
		return crcCombiner{poly: crc32.Castagnoli, width: 32, size: 4}, nil
	case domain.ChecksumCRC64NVME:
		return crcCombiner{poly: crc64NVMEPoly, width: 64, size: 8}, nil
	default:
		return crcCombiner{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The FULL_OBJECT checksum type needs a CRC checksum algorithm, not "+string(algo))
	}
}

// decode reads a big-endian CRC (the S3 encoding of the digest).
func (c crcCombiner) decode(raw []byte) uint64 {
	if c.width == 32 {
		return uint64(binary.BigEndian.Uint32(raw))
	}
	return binary.BigEndian.Uint64(raw)
}

// encode renders a CRC big-endian.
func (c crcCombiner) encode(crc uint64) []byte {
	out := make([]byte, c.size)
	if c.width == 32 {
		binary.BigEndian.PutUint32(out, uint32(crc)) // #nosec G115 -- a 32-bit CRC kept in a uint64
	} else {
		binary.BigEndian.PutUint64(out, crc)
	}
	return out
}

// combine returns the CRC of A||B from crc1 = CRC(A), crc2 = CRC(B) and
// len2 = len(B): crc1 is advanced through len2 zero bytes with the
// operator matrices of GF(2), squared for each bit of len2.
func (c crcCombiner) combine(crc1, crc2 uint64, len2 int64) uint64 {
	if len2 <= 0 {
		return crc1
	}
	even := make([]uint64, c.width)
	odd := make([]uint64, c.width)
	// The operator of one zero bit.
	odd[0] = c.poly
	row := uint64(1)
	for n := 1; n < c.width; n++ {
		odd[n] = row
		row <<= 1
	}
	c.square(even, odd) // two zero bits
	c.square(odd, even) // four zero bits
	for {
		c.square(even, odd)
		if len2&1 != 0 {
			crc1 = gf2Times(even, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
		c.square(odd, even)
		if len2&1 != 0 {
			crc1 = gf2Times(odd, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
	}
	return crc1 ^ crc2
}

// square sets dst to mat squared.
func (c crcCombiner) square(dst, mat []uint64) {
	for n := 0; n < c.width; n++ {
		dst[n] = gf2Times(mat, mat[n])
	}
}

// gf2Times multiplies the GF(2) matrix mat by the vector vec.
func gf2Times(mat []uint64, vec uint64) uint64 {
	var sum uint64
	for i := 0; vec != 0; i, vec = i+1, vec>>1 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
	}
	return sum
}
