package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
)

// maxICCSize bounds an embedded ICC profile (the compressed one of a PNG
// included): a matrix/TRC profile is a few kilobytes, the largest LUT
// profiles a few megabytes.
const maxICCSize = 4 << 20

// meta is what a source says about its picture besides the pixels: its
// EXIF orientation (1 when none) and its color space.
type meta struct {
	orientation int
	color       colorInfo
}

// colorInfo is the color space a source declares, in the ways the formats
// declare it; the zero value is untagged (sRGB).
type colorInfo struct {
	// icc is an embedded ICC profile.
	icc []byte
	// cicp are the color primaries and the transfer characteristics of
	// ITU-T H.273, from an nclx colr box (AVIF, HEIC) or a cICP chunk
	// (PNG), when hasCICP.
	hasCICP             bool
	primaries, transfer uint16
	// srgb is a PNG sRGB chunk.
	srgb bool
	// gamma is the decoding exponent of a PNG gAMA chunk (2.2 for the
	// usual 45455), 0 when none; chrm are the chromaticities of a cHRM
	// chunk (white, red, green, blue x and y), when hasCHRM.
	gamma   float64
	hasCHRM bool
	chrm    [8]float64
}

// sourceMeta reads the orientation and the color space of a source of a
// format (HEIC is read by its decoder).
func sourceMeta(format string, data []byte) meta {
	switch format {
	case JPEG:
		return jpegMeta(data)
	case PNG:
		return pngMeta(data)
	case WebP:
		return webpMeta(data)
	case TIFF:
		return meta{orientation: tiffOrientation(data), color: colorInfo{icc: tiffICC(data)}}
	case AVIF:
		// The orientation (irot, imir) is applied by the decoder.
		return meta{orientation: 1, color: isobmffColor(data)}
	}
	return meta{orientation: 1}
}

// jpegMeta reads the APP1 Exif segment (its orientation) and the APP2
// ICC_PROFILE segments (a profile split in up to 255 chunks, numbered
// from 1) of a JPEG file.
func jpegMeta(data []byte) meta {
	m := meta{orientation: 1}
	var chunks [][]byte
	i := 2 // after SOI
	for i+4 <= len(data) {
		if data[i] != 0xff {
			break
		}
		marker := data[i+1]
		if marker == 0xff { // fill byte
			i++
			continue
		}
		if marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01 {
			i += 2
			continue
		}
		if marker == 0xda || marker == 0xd9 { // SOS, EOI: no metadata after
			break
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			break
		}
		seg := data[i+4 : i+2+n]
		switch {
		case marker == 0xe1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" && m.orientation == 1:
			m.orientation = tiffOrientation(seg[6:])
		case marker == 0xe2 && len(seg) > 14 && string(seg[:12]) == "ICC_PROFILE\x00":
			seq, count := int(seg[12]), int(seg[13])
			if chunks == nil && count > 0 {
				chunks = make([][]byte, count)
			}
			if seq >= 1 && seq <= len(chunks) && count == len(chunks) {
				chunks[seq-1] = seg[14:]
			}
		}
		i += 2 + n
	}
	if len(chunks) > 0 {
		var icc []byte
		for _, c := range chunks {
			if c == nil { // a missing chunk: the profile is unusable
				icc = nil
				break
			}
			icc = append(icc, c...)
		}
		m.color.icc = icc
	}
	return m
}

// pngMeta reads the eXIf, iCCP, cICP, sRGB, gAMA and cHRM chunks of a PNG
// file. cICP comes before the others (PNG third edition), then iCCP, then
// sRGB, then gAMA and cHRM.
func pngMeta(data []byte) meta {
	m := meta{orientation: 1}
	c := &m.color
	for i := 8; i+12 <= len(data); {
		n := int64(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		if int64(i)+12+n > int64(len(data)) {
			break
		}
		body := data[i+8 : i+8+int(n)]
		switch typ {
		case "eXIf":
			m.orientation = tiffOrientation(exifPayload(body))
		case "iCCP":
			c.icc = pngICC(body)
		case "cICP":
			if len(body) == 4 {
				c.hasCICP, c.primaries, c.transfer = true, uint16(body[0]), uint16(body[1])
			}
		case "sRGB":
			c.srgb = true
		case "gAMA":
			if len(body) == 4 {
				if g := binary.BigEndian.Uint32(body); g > 0 {
					c.gamma = 100000 / float64(g)
				}
			}
		case "cHRM":
			if len(body) == 32 {
				c.hasCHRM = true
				for k := range c.chrm {
					c.chrm[k] = float64(binary.BigEndian.Uint32(body[4*k:])) / 100000
				}
			}
		}
		if typ == "IEND" {
			break
		}
		i += 12 + int(n)
	}
	if c.hasCICP {
		c.icc = nil
	}
	return m
}

// pngICC returns the profile of an iCCP chunk: a name, a NUL, the
// compression method (0, zlib) and the compressed profile.
func pngICC(body []byte) []byte {
	k := bytes.IndexByte(body, 0)
	if k < 0 || k+2 > len(body) || body[k+1] != 0 {
		return nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(body[k+2:]))
	if err != nil {
		return nil
	}
	icc, err := io.ReadAll(io.LimitReader(zr, maxICCSize))
	if err != nil {
		return nil
	}
	return icc
}

// webpMeta reads the EXIF and ICCP chunks of an extended WebP file.
func webpMeta(data []byte) meta {
	m := meta{orientation: 1}
	for i := 12; i+8 <= len(data); {
		n := int64(binary.LittleEndian.Uint32(data[i+4:]))
		if int64(i)+8+n > int64(len(data)) {
			break
		}
		body := data[i+8 : i+8+int(n)]
		switch string(data[i : i+4]) {
		case "EXIF":
			m.orientation = tiffOrientation(exifPayload(body))
		case "ICCP":
			m.color.icc = body
		}
		i += 8 + int(n) + int(n&1) // chunks are padded to an even size
	}
	return m
}

// isobmffColor reads the colr properties of the primary item of an HEIF
// file (AVIF): an nclx description, an ICC profile (prof, rICC), or both.
func isobmffColor(data []byte) colorInfo {
	var c colorInfo
	meta := boxPayload(data, "meta")
	if len(meta) < 4 {
		return c
	}
	meta = meta[4:] // a full box
	primary := -1
	if pitm := boxPayload(meta, "pitm"); len(pitm) >= 6 {
		if pitm[0] == 0 {
			primary = int(binary.BigEndian.Uint16(pitm[4:]))
		} else if len(pitm) >= 8 {
			primary = int(binary.BigEndian.Uint32(pitm[4:]))
		}
	}
	iprp := boxPayload(meta, "iprp")
	var props [][]byte // the payloads of ipco, a colr or nil
	eachBox(boxPayload(iprp, "ipco"), func(typ string, payload []byte) {
		if typ == "colr" {
			props = append(props, payload)
		} else {
			props = append(props, nil)
		}
	})
	use := func(colr []byte) {
		if len(colr) < 4 {
			return
		}
		switch string(colr[:4]) {
		case "nclx":
			if len(colr) >= 11 && !c.hasCICP {
				c.hasCICP = true
				c.primaries = binary.BigEndian.Uint16(colr[4:])
				c.transfer = binary.BigEndian.Uint16(colr[6:])
			}
		case "prof", "rICC":
			if c.icc == nil {
				c.icc = colr[4:]
			}
		}
	}
	associated := false
	if primary >= 0 {
		for _, idx := range ipmaIndices(boxPayload(iprp, "ipma"), primary) {
			if idx >= 1 && idx <= len(props) && props[idx-1] != nil {
				associated = true
				use(props[idx-1])
			}
		}
	}
	if !associated {
		for _, p := range props {
			if p != nil {
				use(p)
			}
		}
	}
	return c
}

// eachBox calls fn with the type and the payload of each ISO BMFF box of
// b, stopping at the first malformed one.
func eachBox(b []byte, fn func(typ string, payload []byte)) {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		switch size {
		case 0: // to the end
			size = uint64(len(b))
		case 1: // a 64-bit size
			if len(b) < 16 {
				return
			}
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		}
		if size < hdr || size > uint64(len(b)) {
			return
		}
		fn(typ, b[hdr:size])
		b = b[size:]
	}
}

// boxPayload returns the payload of the first box of a type in b, nil
// when there is none.
func boxPayload(b []byte, typ string) []byte {
	var out []byte
	eachBox(b, func(t string, payload []byte) {
		if out == nil && t == typ {
			out = payload
		}
	})
	return out
}

// ipmaIndices returns the (1-based) ipco indices of the properties an
// ipma box associates with an item.
func ipmaIndices(ipma []byte, item int) []int {
	if len(ipma) < 8 {
		return nil
	}
	version, flags := ipma[0], ipma[3]
	n := int(binary.BigEndian.Uint32(ipma[4:]))
	p := 8
	for range n {
		var id int
		if version < 1 {
			if p+2 > len(ipma) {
				return nil
			}
			id = int(binary.BigEndian.Uint16(ipma[p:]))
			p += 2
		} else {
			if p+4 > len(ipma) {
				return nil
			}
			id = int(binary.BigEndian.Uint32(ipma[p:]))
			p += 4
		}
		if p >= len(ipma) {
			return nil
		}
		count := int(ipma[p])
		p++
		var out []int
		for range count {
			if flags&1 != 0 {
				if p+2 > len(ipma) {
					return nil
				}
				out = append(out, int(binary.BigEndian.Uint16(ipma[p:])&0x7fff))
				p += 2
			} else {
				if p >= len(ipma) {
					return nil
				}
				out = append(out, int(ipma[p]&0x7f))
				p++
			}
		}
		if id == item {
			return out
		}
	}
	return nil
}
