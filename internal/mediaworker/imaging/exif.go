package imaging

import "encoding/binary"

// tagOrientation is the EXIF (TIFF) tag of the orientation.
const tagOrientation = 0x0112

// jpegOrientation returns the EXIF orientation of a JPEG file (1 when it
// has none): the Orientation tag of IFD0 in its APP1 Exif segment.
func jpegOrientation(data []byte) int {
	i := 2 // after SOI
	for i+4 <= len(data) {
		if data[i] != 0xff {
			return 1
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
			return 1
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return 1
		}
		seg := data[i+4 : i+2+n]
		if marker == 0xe1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

// tiffOrientation returns the Orientation tag of IFD0 of a TIFF structure
// (a TIFF file or the payload of an EXIF segment), 1 when it has none or
// an invalid one.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int64(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > int64(len(t)) {
		return 1
	}
	n := int64(bo.Uint16(t[ifd:]))
	for k := range n {
		e := ifd + 2 + k*12
		if e+12 > int64(len(t)) {
			return 1
		}
		if bo.Uint16(t[e:]) != tagOrientation {
			continue
		}
		// A SHORT (3) value is left-justified in the 4 bytes of the entry.
		if bo.Uint16(t[e+2:]) != 3 {
			return 1
		}
		if o := int(bo.Uint16(t[e+8:])); o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}
