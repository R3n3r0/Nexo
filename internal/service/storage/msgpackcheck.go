package storage

import (
	"encoding/binary"
	"errors"
)

// Structural errors of a msgpack document.
var (
	errMsgpackTruncated = errors.New("msgpack: truncated document")
	errMsgpackTrailing  = errors.New("msgpack: trailing bytes after document")
	errMsgpackTooDeep   = errors.New("msgpack: containers nested too deeply")
	errMsgpackTooLarge  = errors.New("msgpack: container length exceeds the document")
	errMsgpackCode      = errors.New("msgpack: invalid type code")
)

// checkMsgpack verifies that b holds exactly one well-formed msgpack value
// whose containers are nested at most maxDepth levels deep and whose
// declared lengths fit in the document. The reflection decoder allocates
// containers from their declared length before reading the elements, so a
// corrupted length (0xdd ffffffff) would otherwise allocate gigabytes:
// after this check every allocation is bounded by the document size.
func checkMsgpack(b []byte, maxDepth int) error {
	// pending[i] is the number of values still expected at depth i.
	pending := make([]uint64, 1, 8)
	pending[0] = 1
	pos := 0
	for len(pending) > 0 {
		top := len(pending) - 1
		if pending[top] == 0 {
			pending = pending[:top]
			continue
		}
		pending[top]--
		if pos >= len(b) {
			return errMsgpackTruncated
		}
		c := b[pos]
		pos++
		var skip, items uint64
		switch {
		case c <= 0x7f || c >= 0xe0 || c == 0xc0 || c == 0xc2 || c == 0xc3:
			// fixint, nil, false, true: no payload.
		case c >= 0x80 && c <= 0x8f: // fixmap
			items = 2 * uint64(c&0x0f)
		case c >= 0x90 && c <= 0x9f: // fixarray
			items = uint64(c & 0x0f)
		case c >= 0xa0 && c <= 0xbf: // fixstr
			skip = uint64(c & 0x1f)
		default:
			var n uint64
			var err error
			switch c {
			case 0xc4, 0xd9: // bin8, str8
				n, err = readLength(b, &pos, 1)
				skip = n
			case 0xc5, 0xda: // bin16, str16
				n, err = readLength(b, &pos, 2)
				skip = n
			case 0xc6, 0xdb: // bin32, str32
				n, err = readLength(b, &pos, 4)
				skip = n
			case 0xc7: // ext8: length, type, data
				n, err = readLength(b, &pos, 1)
				skip = n + 1
			case 0xc8: // ext16
				n, err = readLength(b, &pos, 2)
				skip = n + 1
			case 0xc9: // ext32
				n, err = readLength(b, &pos, 4)
				skip = n + 1
			case 0xcc, 0xd0: // uint8, int8
				skip = 1
			case 0xcd, 0xd1: // uint16, int16
				skip = 2
			case 0xca, 0xce, 0xd2: // float32, uint32, int32
				skip = 4
			case 0xcb, 0xcf, 0xd3: // float64, uint64, int64
				skip = 8
			case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8: // fixext 1, 2, 4, 8, 16 (+ type)
				skip = 1 + 1<<(c-0xd4)
			case 0xdc: // array16
				items, err = readLength(b, &pos, 2)
			case 0xdd: // array32
				items, err = readLength(b, &pos, 4)
			case 0xde: // map16
				n, err = readLength(b, &pos, 2)
				items = 2 * n
			case 0xdf: // map32
				n, err = readLength(b, &pos, 4)
				items = 2 * n
			default: // 0xc1 is never used
				return errMsgpackCode
			}
			if err != nil {
				return err
			}
		}
		remaining := uint64(len(b) - pos) // #nosec G115 -- pos never exceeds len(b)
		if skip > remaining {
			return errMsgpackTruncated
		}
		pos += int(skip)
		if items > 0 {
			// Every value takes at least one byte.
			// #nosec G115 -- pos never exceeds len(b)
			if items > uint64(len(b)-pos) {
				return errMsgpackTooLarge
			}
			if len(pending) > maxDepth {
				return errMsgpackTooDeep
			}
			pending = append(pending, items)
		}
	}
	if pos != len(b) {
		return errMsgpackTrailing
	}
	return nil
}

// readLength reads a big endian length of size bytes at *pos.
func readLength(b []byte, pos *int, size int) (uint64, error) {
	if len(b)-*pos < size {
		return 0, errMsgpackTruncated
	}
	var n uint64
	switch size {
	case 1:
		n = uint64(b[*pos])
	case 2:
		n = uint64(binary.BigEndian.Uint16(b[*pos:]))
	default:
		n = uint64(binary.BigEndian.Uint32(b[*pos:]))
	}
	*pos += size
	return n, nil
}
