package s3

import (
	"net/url"
	"strings"
)

// encodingTypeURL is the only encoding-type of the listings.
const encodingTypeURL = "url"

// listEncoder applies the encoding-type of a listing to the keys and
// prefixes of its response.
type listEncoder struct {
	enabled bool
}

// newListEncoder validates the encoding-type parameter.
func newListEncoder(q url.Values) (listEncoder, error) {
	switch v := q.Get("encoding-type"); v {
	case "":
		return listEncoder{}, nil
	case encodingTypeURL:
		return listEncoder{enabled: true}, nil
	default:
		return listEncoder{}, invalidArgument("Invalid Encoding Method specified in Request")
	}
}

// Type returns the EncodingType element of the response.
func (e listEncoder) Type() string {
	if e.enabled {
		return encodingTypeURL
	}
	return ""
}

// Encode encodes s when the listing asked for it.
func (e listEncoder) Encode(s string) string {
	if !e.enabled {
		return s
	}
	return s3URLEncode(s)
}

// s3URLEncode is the URL encoding of S3 listings: the unreserved
// characters, "/" and "*" are kept as is, every other byte is
// percent-encoded, the space included ("%20", never "+"), so that both a
// query unescape (the SDKs) and a path unescape decode it (Ceph s3-tests
// test_bucket_list_encoding_basic).
func s3URLEncode(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) * 3 / 2)
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '/', c == '*':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
