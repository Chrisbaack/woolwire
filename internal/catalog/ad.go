package catalog

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Chrisbaack/woolwire/internal/identity"
)

const StaleAdTimeoutSeconds = 90

type ModelAd struct {
	RoomID        string `json:"room_id"`
	HostMemberID  string `json:"host_member_id"`
	ModelID       string `json:"model_id"`
	Revision      int    `json:"revision"`
	Name          string `json:"name"`
	Quantization  string `json:"quantization,omitempty"`
	ContextLimit  int    `json:"context_limit"`
	Availability  string `json:"availability"` // "ready", "unloaded", "busy", "offline"
	QueueEstimate int    `json:"queue_estimate"`
	IsManaged     bool   `json:"is_managed"`
	// SupportsThinking tells a requester that this model can be asked to
	// reason, so the chat view can offer a thinking level for it.
	//
	// It is deliberately outside Payload, and so unsigned. Adding a field to
	// the canonical payload would make every advertisement from an updated
	// host fail verification on a peer that has not updated, and the field is
	// not worth that: a catalog read accepts an advertisement only from the
	// member the handshake proved owns it, so nobody else is in a position to
	// alter it, and the worst a host could do by lying is offer its own
	// requesters a control the model ignores.
	SupportsThinking bool   `json:"supports_thinking,omitempty"`
	Timestamp        int64  `json:"timestamp"`
	Signature        string `json:"signature"`
	// SigVersion selects the signing payload format; see identity.SigningPayload.
	SigVersion uint8 `json:"sig_version,omitempty"`
}

func (m ModelAd) Payload() []byte {
	if m.SigVersion == 0 {
		// Legacy colon-joined payload. Model names contain colons routinely
		// (llama3:latest), so this form is ambiguous and is kept only to
		// verify existing advertisements.
		return []byte(fmt.Sprintf("%s:%s:%s:%d:%s:%s:%d:%s:%d:%t:%d",
			m.RoomID, m.HostMemberID, m.ModelID, m.Revision, m.Name,
			m.Quantization, m.ContextLimit, m.Availability, m.QueueEstimate,
			m.IsManaged, m.Timestamp))
	}
	return identity.SigningPayload(m.SigVersion, "woolwire/model-ad",
		m.RoomID, m.HostMemberID, m.ModelID, m.Revision, m.Name,
		m.Quantization, m.ContextLimit, m.Availability, m.QueueEstimate,
		m.IsManaged, m.Timestamp)
}

func (m *ModelAd) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid private key")
	}
	m.Timestamp = time.Now().Unix()
	m.SigVersion = identity.SigVersionCanonical
	sig := ed25519.Sign(privateKey, m.Payload())
	m.Signature = identity.EncodeToken(sig)
	return nil
}

func (m ModelAd) Verify(publicKey ed25519.PublicKey) error {
	if m.RoomID == "" || m.HostMemberID == "" || m.ModelID == "" || m.Name == "" {
		return errors.New("incomplete model advertisement")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid public key")
	}
	sig, err := identity.DecodeToken(m.Signature, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(publicKey, m.Payload(), sig) {
		return errors.New("model advertisement signature verification failed")
	}
	return nil
}

type Catalog struct {
	mu  sync.RWMutex
	ads map[string]ModelAd
}

func NewCatalog() *Catalog {
	return &Catalog{
		ads: make(map[string]ModelAd),
	}
}

func adKey(hostMemberID, modelID string) string {
	return hostMemberID + "/" + modelID
}

func (c *Catalog) Upsert(ad ModelAd, hostPublicKey ed25519.PublicKey) error {
	if err := ad.Verify(hostPublicKey); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := adKey(ad.HostMemberID, ad.ModelID)
	existing, ok := c.ads[key]
	if ok && ad.Revision < existing.Revision {
		// Reject stale revisions
		return errors.New("rejected older revision advertisement")
	}
	c.ads[key] = ad
	return nil
}

func (c *Catalog) ListAvailable() []ModelAd {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now().Unix()
	var available []ModelAd
	for _, ad := range c.ads {
		// Stale check: ads older than 90 seconds are ignored
		if now-ad.Timestamp > StaleAdTimeoutSeconds {
			continue
		}
		if ad.Availability == "offline" {
			continue
		}
		available = append(available, ad)
	}
	return available
}

// List returns the current advertisements, including offline ones. Local
// refresh uses it to publish a signed offline tombstone immediately when a
// prepared file is removed instead of waiting for the stale-ad timeout.
func (c *Catalog) List() []ModelAd {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelAd, 0, len(c.ads))
	for _, ad := range c.ads {
		out = append(out, ad)
	}
	return out
}

func (c *Catalog) Get(hostMemberID, modelID string) (ModelAd, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ad, ok := c.ads[adKey(hostMemberID, modelID)]
	if !ok {
		return ModelAd{}, false
	}
	if time.Now().Unix()-ad.Timestamp > StaleAdTimeoutSeconds {
		ad.Availability = "offline"
	}
	return ad, true
}
