package identity

import (
	"bytes"
	"encoding/binary"
	"strconv"
)

// SigVersionCanonical is the current signing-payload format. Records written
// before it carry sig_version 0 and are verified with the legacy colon-joined
// payload, which is why the field is stored rather than assumed.
const SigVersionCanonical = 1

// SigningPayload builds an unambiguous byte string to sign. Every field is
// length-prefixed and the whole payload is domain-separated, so no combination
// of display names, model names, or message bodies containing separators can
// produce the same bytes as a different record. The colon-joined form it
// replaces had no such guarantee.
func SigningPayload(version uint8, domain string, fields ...any) []byte {
	var buf bytes.Buffer
	buf.WriteByte(version)
	writeField(&buf, []byte(domain))
	for _, f := range fields {
		writeField(&buf, fieldBytes(f))
	}
	return buf.Bytes()
}

func fieldBytes(f any) []byte {
	switch v := f.(type) {
	case string:
		return []byte(v)
	case []byte:
		return v
	case int:
		return []byte(strconv.FormatInt(int64(v), 10))
	case int64:
		return []byte(strconv.FormatInt(v, 10))
	case bool:
		if v {
			return []byte{1}
		}
		return []byte{0}
	default:
		return nil
	}
}

func writeField(buf *bytes.Buffer, b []byte) {
	var header [8]byte
	binary.BigEndian.PutUint64(header[:], uint64(len(b)))
	buf.Write(header[:])
	buf.Write(b)
}
