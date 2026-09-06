package community

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// FuzzEventVerify checks that verification never panics and never accepts an
// event whose signature and ID were not produced by Sign.
func FuzzEventVerify(f *testing.F) {
	f.Add("evt-1", "room-1", "chan-1", "m-1", int64(1), "message", "", "hi", int64(1), "sig", uint8(1))
	f.Add("", "", "", "", int64(0), "", "", "", int64(0), "", uint8(0))
	f.Add("evt-1", "room:1", "chan:1", "m:1", int64(1), "message", "", "a:b:c", int64(1), "sig", uint8(0))

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, id, roomID, channelID, author string, seq int64,
		eventType, target, content string, timestamp int64, signature string, sigVersion uint8) {
		e := Event{
			ID:             id,
			RoomID:         roomID,
			ChannelID:      channelID,
			AuthorMemberID: author,
			AuthorSeq:      seq,
			EventType:      EventType(eventType),
			TargetEventID:  target,
			Content:        content,
			Timestamp:      timestamp,
			Signature:      signature,
			SigVersion:     sigVersion,
		}
		// Must not panic; an arbitrary signature must not verify.
		if err := e.Verify(pub); err == nil {
			t.Fatalf("a fuzzed event verified: %#v", e)
		}

		// Signing the same fields always produces an event that verifies, and
		// materializing it never panics.
		if roomID != "" && channelID != "" && author != "" && seq > 0 {
			signed := e
			signed.EventType = EventMessage
			if err := signed.Sign(priv); err != nil {
				t.Fatalf("sign failed: %v", err)
			}
			if err := signed.Verify(pub); err != nil {
				t.Fatalf("a freshly signed event failed to verify: %v", err)
			}
			_ = Materialize([]Event{signed})
		}
	})
}

// FuzzSanitizeContent checks that sanitization terminates, never panics, and
// only ever removes markdown image syntax.
func FuzzSanitizeContent(f *testing.F) {
	for _, s := range []string{
		"", "a < b & c", "![alt](https://x/y.png)", "![](x)", "!]([",
		"<script>", "I <3 you", "![a](b)![c](d)",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := SanitizeContent(raw)
		// Text with no markdown image syntax must survive byte for byte:
		// escaping it here corrupted stored messages permanently.
		if !containsImageSyntax(raw) && got != raw {
			t.Fatalf("SanitizeContent(%q) = %q, want it unchanged", raw, got)
		}
	})
}

func containsImageSyntax(s string) bool {
	return markdownImgRegex.MatchString(s)
}
