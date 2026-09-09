package hosting

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ggufKV is one metadata entry for the synthetic files these tests build.
type ggufKV struct {
	key string
	typ uint32
	val any
}

func kvString(key, val string) ggufKV   { return ggufKV{key, ggufString, val} }
func kvU32(key string, v uint32) ggufKV { return ggufKV{key, ggufUint32, v} }
func kvStrArray(key string, vals []string) ggufKV {
	return ggufKV{key, ggufArray, vals}
}

func writeGGUFValue(buf *bytes.Buffer, typ uint32, val any) {
	switch typ {
	case ggufString:
		s := val.(string)
		_ = binary.Write(buf, binary.LittleEndian, uint64(len(s)))
		buf.WriteString(s)
	case ggufUint32:
		_ = binary.Write(buf, binary.LittleEndian, val.(uint32))
	case ggufArray:
		vals := val.([]string)
		_ = binary.Write(buf, binary.LittleEndian, ggufString)
		_ = binary.Write(buf, binary.LittleEndian, uint64(len(vals)))
		for _, s := range vals {
			_ = binary.Write(buf, binary.LittleEndian, uint64(len(s)))
			buf.WriteString(s)
		}
	default:
		panic("unsupported test value type")
	}
}

// writeGGUF builds a file with a valid GGUF header and no tensor data.
func writeGGUF(t *testing.T, path string, kvs ...ggufKV) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(0)) // tensor count
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	for _, kv := range kvs {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kv.key)))
		buf.WriteString(kv.key)
		_ = binary.Write(&buf, binary.LittleEndian, kv.typ)
		writeGGUFValue(&buf, kv.typ, kv.val)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// languageModelKVs is the metadata a chat model carries: an architecture, a
// context length, and a tokenizer.
func languageModelKVs(arch, name string, ctx uint32) []ggufKV {
	return []ggufKV{
		kvString("general.architecture", arch),
		kvString("general.name", name),
		kvU32(arch+".context_length", ctx),
		kvU32(arch+".block_count", 32),
		kvStrArray("tokenizer.ggml.tokens", []string{"<s>", "hello", "world"}),
		kvString("tokenizer.ggml.model", "gpt2"),
	}
}

func TestReadGGUFMetadataReadsLanguageModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	writeGGUF(t, path, languageModelKVs("qwen3", "Qwen3 8B", 32768)...)

	md, err := readGGUFMetadata(path)
	if err != nil {
		t.Fatalf("readGGUFMetadata returned %v", err)
	}
	if md.Architecture != "qwen3" {
		t.Fatalf("architecture = %q", md.Architecture)
	}
	if md.Name != "Qwen3 8B" {
		t.Fatalf("name = %q", md.Name)
	}
	if md.ContextLength != 32768 {
		t.Fatalf("context length = %d", md.ContextLength)
	}
	if !md.HasTokenizer {
		t.Fatal("expected a tokenizer")
	}
}

// A models directory shared with other tools holds GGUF files that are not
// language models at all: diffusion weights and vision projectors ship in the
// same container format and llama-server cannot serve either.
func TestReadGGUFMetadataRejectsNonLanguageModels(t *testing.T) {
	dir := t.TempDir()

	diffusion := filepath.Join(dir, "flow_dit_Q8_0.gguf")
	writeGGUF(t, diffusion,
		kvString("general.architecture", "flux"),
		kvU32("flux.in_channels", 64),
	)
	if _, err := readGGUFMetadata(diffusion); !errors.Is(err, errGGUFNotModel) {
		t.Fatalf("diffusion weights returned %v, want errGGUFNotModel", err)
	}

	projector := filepath.Join(dir, "mmproj-F16.gguf")
	writeGGUF(t, projector,
		kvString("general.architecture", "clip"),
		kvU32("clip.vision.block_count", 24),
	)
	if _, err := readGGUFMetadata(projector); !errors.Is(err, errGGUFNotModel) {
		t.Fatalf("projector returned %v, want errGGUFNotModel", err)
	}
}

func TestReadGGUFMetadataRejectsMalformedFiles(t *testing.T) {
	dir := t.TempDir()

	notGGUF := filepath.Join(dir, "notes.gguf")
	if err := os.WriteFile(notGGUF, []byte("this is not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUFMetadata(notGGUF); err == nil {
		t.Fatal("expected an error for a non-GGUF file")
	}

	// A header that claims far more metadata than it contains must fail
	// rather than read to exhaustion.
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(1<<40))
	truncated := filepath.Join(dir, "truncated.gguf")
	if err := os.WriteFile(truncated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUFMetadata(truncated); err == nil {
		t.Fatal("expected an error for an oversized metadata count")
	}

	// A string length larger than the file must fail too.
	var big bytes.Buffer
	big.WriteString("GGUF")
	_ = binary.Write(&big, binary.LittleEndian, uint32(3))
	_ = binary.Write(&big, binary.LittleEndian, uint64(0))
	_ = binary.Write(&big, binary.LittleEndian, uint64(1))
	_ = binary.Write(&big, binary.LittleEndian, uint64(1<<40))
	oversized := filepath.Join(dir, "oversized.gguf")
	if err := os.WriteFile(oversized, big.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUFMetadata(oversized); err == nil {
		t.Fatal("expected an error for an oversized key length")
	}

	unsupported := filepath.Join(dir, "v1.gguf")
	var v1 bytes.Buffer
	v1.WriteString("GGUF")
	_ = binary.Write(&v1, binary.LittleEndian, uint32(1))
	if err := os.WriteFile(unsupported, v1.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUFMetadata(unsupported); err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
}

// TestChatTemplateThinkingDetection pins the signal the chat UI depends on.
// Nothing else in a GGUF header says whether a model reasons, and each family
// spells the template branch differently.
func TestChatTemplateThinkingDetection(t *testing.T) {
	reasoning := map[string]string{
		"qwen3":      `{%- if enable_thinking is defined and enable_thinking %}<think>{%- endif %}`,
		"gpt-oss":    `{{- "Reasoning: " + reasoning_effort + "\n\n" }}`,
		"deepseek":   `{%- if add_generation_prompt %}{{'<think>\n'}}{%- endif %}`,
		"mixed-case": `{%- if ENABLE_THINKING %}x{%- endif %}`,
	}
	for name, tmpl := range reasoning {
		if !chatTemplateSupportsThinking(tmpl) {
			t.Errorf("%s template was not recognized as a reasoning one", name)
		}
	}

	plain := map[string]string{
		"llama":     `{%- for message in messages %}{{ message.content }}{%- endfor %}`,
		"empty":     ``,
		"near-miss": `{{ "I have been thinking about lunch" }}`,
	}
	for name, tmpl := range plain {
		if chatTemplateSupportsThinking(tmpl) {
			t.Errorf("%s template was wrongly treated as a reasoning one", name)
		}
	}
}
