package hosting

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Enough of the GGUF container to answer three questions about a file we found
// on disk: is it a language model, what is it called, and how much context was
// it trained for. Only the header is read — never the tensor data.
//
// Format: "GGUF", uint32 version, uint64 tensor count, uint64 metadata count,
// then that many key/value pairs. Values are typed, and every type carries its
// own length, so unknown keys can be skipped without understanding them.

const (
	ggufMagic = "GGUF"

	// Bounds on a file we have not validated yet. A corrupt or hostile header
	// must not be able to make us allocate or read without limit.
	maxGGUFKeyValues  = 1 << 16
	maxGGUFStringLen  = 1 << 24 // 16 MiB
	maxGGUFArrayCount = 1 << 26
	maxGGUFHeaderScan = 512 << 20
)

// GGUF value type tags.
const (
	ggufUint8 uint32 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

var errGGUFNotModel = errors.New("gguf file is not a language model")

// ggufMetadata is the subset of the header the model list cares about.
type ggufMetadata struct {
	Architecture  string
	Name          string
	ContextLength int
	// HasTokenizer separates a language model from the other things shipped as
	// GGUF — diffusion weights, CLIP projectors, embedding-only exports. A
	// tokenizer is what llama-server needs and what none of those carry.
	HasTokenizer bool
}

type ggufReader struct {
	br   *bufio.Reader
	read int64
}

func (r *ggufReader) take(n int64) error {
	r.read += n
	if r.read > maxGGUFHeaderScan {
		return errors.New("gguf header exceeds scan budget")
	}
	return nil
}

func (r *ggufReader) bytes(n int64) ([]byte, error) {
	if err := r.take(n); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (r *ggufReader) skip(n int64) error {
	if n < 0 {
		return errors.New("gguf negative length")
	}
	if err := r.take(n); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, r.br, n)
	return err
}

func (r *ggufReader) u32() (uint32, error) {
	b, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *ggufReader) u64() (uint64, error) {
	b, err := r.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (r *ggufReader) str() (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > maxGGUFStringLen {
		return "", fmt.Errorf("gguf string of %d bytes exceeds limit", n)
	}
	b, err := r.bytes(int64(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// scalarSize is the fixed width of a non-string, non-array value.
func scalarSize(typ uint32) (int64, bool) {
	switch typ {
	case ggufUint8, ggufInt8, ggufBool:
		return 1, true
	case ggufUint16, ggufInt16:
		return 2, true
	case ggufUint32, ggufInt32, ggufFloat32:
		return 4, true
	case ggufUint64, ggufInt64, ggufFloat64:
		return 8, true
	}
	return 0, false
}

// skipValue consumes one value of the given type without interpreting it.
func (r *ggufReader) skipValue(typ uint32) error {
	if size, ok := scalarSize(typ); ok {
		return r.skip(size)
	}
	switch typ {
	case ggufString:
		n, err := r.u64()
		if err != nil {
			return err
		}
		if n > maxGGUFStringLen {
			return fmt.Errorf("gguf string of %d bytes exceeds limit", n)
		}
		return r.skip(int64(n))
	case ggufArray:
		elemType, err := r.u32()
		if err != nil {
			return err
		}
		count, err := r.u64()
		if err != nil {
			return err
		}
		if count > maxGGUFArrayCount {
			return fmt.Errorf("gguf array of %d elements exceeds limit", count)
		}
		if size, ok := scalarSize(elemType); ok {
			return r.skip(int64(count) * size)
		}
		if elemType != ggufString {
			// GGUF has no nested arrays; anything else is a malformed header.
			return fmt.Errorf("gguf unsupported array element type %d", elemType)
		}
		// A token vocabulary is a string array of ~150k entries, so each
		// length has to be read to find the next one.
		for i := uint64(0); i < count; i++ {
			n, err := r.u64()
			if err != nil {
				return err
			}
			if n > maxGGUFStringLen {
				return fmt.Errorf("gguf string of %d bytes exceeds limit", n)
			}
			if err := r.skip(int64(n)); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("gguf unknown value type %d", typ)
}

// readUintValue reads a value known to hold a non-negative integer.
func (r *ggufReader) readUintValue(typ uint32) (uint64, error) {
	switch typ {
	case ggufUint8, ggufInt8:
		b, err := r.bytes(1)
		if err != nil {
			return 0, err
		}
		return uint64(b[0]), nil
	case ggufUint16, ggufInt16:
		b, err := r.bytes(2)
		if err != nil {
			return 0, err
		}
		return uint64(binary.LittleEndian.Uint16(b)), nil
	case ggufUint32, ggufInt32:
		v, err := r.u32()
		return uint64(v), err
	case ggufUint64, ggufInt64:
		return r.u64()
	}
	return 0, r.skipValue(typ)
}

// readGGUFMetadata reads the header of a GGUF file. It returns errGGUFNotModel
// for a well-formed GGUF that is not a language model.
func readGGUFMetadata(path string) (ggufMetadata, error) {
	var md ggufMetadata

	f, err := os.Open(path)
	if err != nil {
		return md, err
	}
	defer f.Close()

	r := &ggufReader{br: bufio.NewReaderSize(f, 64<<10)}

	magic, err := r.bytes(4)
	if err != nil {
		return md, err
	}
	if string(magic) != ggufMagic {
		return md, errors.New("not a gguf file")
	}

	version, err := r.u32()
	if err != nil {
		return md, err
	}
	// v1 sized its lengths differently and predates every model anyone still
	// runs; v2 and v3 share this layout.
	if version != 2 && version != 3 {
		return md, fmt.Errorf("unsupported gguf version %d", version)
	}
	if _, err := r.u64(); err != nil { // tensor count
		return md, err
	}
	kvCount, err := r.u64()
	if err != nil {
		return md, err
	}
	if kvCount > maxGGUFKeyValues {
		return md, fmt.Errorf("gguf declares %d metadata entries", kvCount)
	}

	for i := uint64(0); i < kvCount; i++ {
		key, err := r.str()
		if err != nil {
			return md, err
		}
		typ, err := r.u32()
		if err != nil {
			return md, err
		}

		switch {
		case key == "general.architecture":
			if typ != ggufString {
				if err := r.skipValue(typ); err != nil {
					return md, err
				}
				continue
			}
			if md.Architecture, err = r.str(); err != nil {
				return md, err
			}
		case key == "general.name":
			if typ != ggufString {
				if err := r.skipValue(typ); err != nil {
					return md, err
				}
				continue
			}
			if md.Name, err = r.str(); err != nil {
				return md, err
			}
		case strings.HasSuffix(key, ".context_length"):
			v, err := r.readUintValue(typ)
			if err != nil {
				return md, err
			}
			if v > 0 && v <= 1<<31 {
				md.ContextLength = int(v)
			}
		default:
			if strings.HasPrefix(key, "tokenizer.ggml.") {
				md.HasTokenizer = true
			}
			if err := r.skipValue(typ); err != nil {
				return md, err
			}
		}
	}

	if !md.HasTokenizer {
		return md, errGGUFNotModel
	}
	return md, nil
}
