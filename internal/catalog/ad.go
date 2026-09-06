package catalog

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
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
	Availability  string `json:"availability"` // "ready", "busy", "offline"
	QueueEstimate int    `json:"queue_estimate"`
	IsManaged     bool   `json:"is_managed"`
	Timestamp     int64  `json:"timestamp"`
	Signature     string `json:"signature"`
}

func (m ModelAd) Payload() []byte {
	return []byte(fmt.Sprintf("%s:%s:%s:%d:%s:%s:%d:%s:%d:%t:%d",
		m.RoomID, m.HostMemberID, m.ModelID, m.Revision, m.Name,
		m.Quantization, m.ContextLimit, m.Availability, m.QueueEstimate,
		m.IsManaged, m.Timestamp))
}

func (m *ModelAd) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid private key")
	}
	m.Timestamp = time.Now().Unix()
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
