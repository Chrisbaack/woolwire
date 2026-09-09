package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("record not found")
)

type Store struct {
	db     *sql.DB
	dbPath string
	mu     sync.RWMutex
}

func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	s := &Store{db: db, dbPath: dbPath}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	// The database holds device and authority private keys, so it must never
	// be group- or world-readable regardless of the process umask. The WAL and
	// shared-memory sidecars carry the same content.
	if dbPath != ":memory:" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, statErr := os.Stat(dbPath + suffix); statErr == nil {
				_ = os.Chmod(dbPath+suffix, 0o600)
			}
		}
	}

	return s, nil
}

func (s *Store) DatabaseSize() int64 {
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return 0
	}
	info, err := os.Stat(s.dbPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) Backup(destPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(destPath)
	_, err := s.db.Exec("VACUUM INTO ?", destPath)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(migrations[0]); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for version, migration := range migrations[1:] {
		v := version + 1
		var exists int
		err := tx.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", v).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration version %d: %w", v, err)
		}
		if exists == 0 {
			if _, err := tx.Exec(migration); err != nil {
				return fmt.Errorf("apply migration %d: %w", v, err)
			}
			if _, err := tx.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", v, time.Now().Unix()); err != nil {
				return fmt.Errorf("record migration %d: %w", v, err)
			}
		}
	}

	return tx.Commit()
}

// Local Settings
func (s *Store) GetSetting(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var val string
	err := s.db.QueryRow("SELECT value FROM local_settings WHERE key = ?", key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return val, err
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO local_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

// Device Identity
type DeviceIdentity struct {
	TailcatKey    string
	DevicePrivate []byte
	DeviceCertDER []byte
	DevicePublic  string
	// TailcatPSK and TailcatAddr must survive restarts together with
	// TailcatKey: the pre-shared key and the pinned DERP region are both
	// embedded in the address peers stored and the invitation code carries.
	TailcatPSK  string
	TailcatAddr string
}

func (s *Store) GetDeviceIdentity() (*DeviceIdentity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var id DeviceIdentity
	err := s.db.QueryRow(`
		SELECT tailcat_key, device_private, device_cert_der, device_public,
		       tailcat_psk, tailcat_addr
		FROM device_identity WHERE id = 1
	`).Scan(&id.TailcatKey, &id.DevicePrivate, &id.DeviceCertDER, &id.DevicePublic,
		&id.TailcatPSK, &id.TailcatAddr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Store) SaveDeviceIdentity(id DeviceIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO device_identity (
			id, tailcat_key, device_private, device_cert_der, device_public,
			tailcat_psk, tailcat_addr
		) VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tailcat_key = excluded.tailcat_key,
			device_private = excluded.device_private,
			device_cert_der = excluded.device_cert_der,
			device_public = excluded.device_public,
			tailcat_psk = excluded.tailcat_psk,
			tailcat_addr = excluded.tailcat_addr
	`, id.TailcatKey, id.DevicePrivate, id.DeviceCertDER, id.DevicePublic,
		id.TailcatPSK, id.TailcatAddr)
	return err
}

// Room State
type RoomRecord struct {
	RoomID              string
	Role                string
	RoomName            string
	AuthorityPublic     string
	AuthorityPrivate    []byte
	BootstrapAddr       string
	InvitationCode      string
	InvitationID        string
	AdmissionSecretHash string
	ApprovalMode        bool
	RosterVersion       int64
	// RoomCertDER is the creator's authority-signed bootstrap certificate.
	// Joiners pin it from the invitation's authority key before they have a
	// roster to authenticate anyone with.
	RoomCertDER []byte
}

func (s *Store) GetRoomState() (*RoomRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var r RoomRecord
	var approvalInt int
	err := s.db.QueryRow(`
		SELECT room_id, role, room_name, authority_public, authority_private,
		       bootstrap_addr, invitation_code, invitation_id, admission_secret_hash,
		       approval_mode, roster_version, room_cert_der
		FROM room_state ORDER BY rowid ASC LIMIT 1
	`).Scan(
		&r.RoomID, &r.Role, &r.RoomName, &r.AuthorityPublic, &r.AuthorityPrivate,
		&r.BootstrapAddr, &r.InvitationCode, &r.InvitationID, &r.AdmissionSecretHash,
		&approvalInt, &r.RosterVersion, &r.RoomCertDER,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ApprovalMode = approvalInt != 0
	return &r, nil
}

func (s *Store) SaveRoomState(r RoomRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	approvalInt := 0
	if r.ApprovalMode {
		approvalInt = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO room_state (
			room_id, role, room_name, authority_public, authority_private,
			bootstrap_addr, invitation_code, invitation_id, admission_secret_hash,
			approval_mode, roster_version, room_cert_der
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(room_id) DO UPDATE SET
			role = excluded.role,
			room_name = excluded.room_name,
			authority_public = excluded.authority_public,
			authority_private = excluded.authority_private,
			bootstrap_addr = excluded.bootstrap_addr,
			invitation_code = excluded.invitation_code,
			invitation_id = excluded.invitation_id,
			admission_secret_hash = excluded.admission_secret_hash,
			approval_mode = excluded.approval_mode,
			roster_version = excluded.roster_version,
			room_cert_der = excluded.room_cert_der
	`, r.RoomID, r.Role, r.RoomName, r.AuthorityPublic, r.AuthorityPrivate,
		r.BootstrapAddr, r.InvitationCode, r.InvitationID, r.AdmissionSecretHash,
		approvalInt, r.RosterVersion, r.RoomCertDER)
	return err
}

func (s *Store) ClearRoom() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM room_state"); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM members"); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM peer_addresses"); err != nil {
		return err
	}
	return tx.Commit()
}

// Members
type MemberRecord struct {
	MemberID      string `json:"member_id"`
	RoomID        string `json:"room_id"`
	DevicePublic  string `json:"device_public"`
	DisplayName   string `json:"display_name"`
	Status        string `json:"status"`
	RosterVersion int64  `json:"roster_version"`
	Signature     string `json:"signature,omitempty"`
	SigVersion    uint8  `json:"sig_version"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func (s *Store) SaveMember(m MemberRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	_, err := s.db.Exec(`
		INSERT INTO members (
			member_id, room_id, device_public, display_name, status,
			roster_version, signature, created_at, updated_at, sig_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(member_id) DO UPDATE SET
			display_name = excluded.display_name,
			status = excluded.status,
			roster_version = excluded.roster_version,
			signature = excluded.signature,
			sig_version = excluded.sig_version,
			updated_at = excluded.updated_at
	`, m.MemberID, m.RoomID, m.DevicePublic, m.DisplayName, m.Status,
		m.RosterVersion, m.Signature, m.CreatedAt, m.UpdatedAt, m.SigVersion)
	return err
}

func (s *Store) GetMember(memberID string) (*MemberRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var m MemberRecord
	err := s.db.QueryRow(`
		SELECT member_id, room_id, device_public, display_name, status,
		       roster_version, signature, created_at, updated_at, sig_version
		FROM members WHERE member_id = ?
	`, memberID).Scan(
		&m.MemberID, &m.RoomID, &m.DevicePublic, &m.DisplayName, &m.Status,
		&m.RosterVersion, &m.Signature, &m.CreatedAt, &m.UpdatedAt, &m.SigVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) ListMembers(roomID string) ([]MemberRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT member_id, room_id, device_public, display_name, status,
		       roster_version, signature, created_at, updated_at, sig_version
		FROM members WHERE room_id = ? ORDER BY display_name ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []MemberRecord
	for rows.Next() {
		var m MemberRecord
		if err := rows.Scan(
			&m.MemberID, &m.RoomID, &m.DevicePublic, &m.DisplayName, &m.Status,
			&m.RosterVersion, &m.Signature, &m.CreatedAt, &m.UpdatedAt, &m.SigVersion,
		); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// Peer Addresses
type PeerAddressRecord struct {
	MemberID    string
	TailcatAddr string
	LastSeen    int64
}

func (s *Store) SavePeerAddress(memberID, tailcatAddr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO peer_addresses (member_id, tailcat_addr, last_seen)
		VALUES (?, ?, ?)
		ON CONFLICT(member_id) DO UPDATE SET
			tailcat_addr = excluded.tailcat_addr,
			last_seen = excluded.last_seen
	`, memberID, tailcatAddr, now)
	return err
}

func (s *Store) ListPeerAddresses() ([]PeerAddressRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT member_id, tailcat_addr, last_seen FROM peer_addresses")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []PeerAddressRecord
	for rows.Next() {
		var r PeerAddressRecord
		if err := rows.Scan(&r.MemberID, &r.TailcatAddr, &r.LastSeen); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) DeletePeerAddress(memberID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM peer_addresses WHERE member_id = ?", memberID)
	return err
}

// Conversations & Messages (My chats)
type ConversationRecord struct {
	ID        string
	Title     string
	NoSave    bool
	CreatedAt int64
	UpdatedAt int64
}

func (s *Store) SaveConversation(c ConversationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now

	noSaveInt := 0
	if c.NoSave {
		noSaveInt = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO conversations (id, title, no_save, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			no_save = excluded.no_save,
			updated_at = excluded.updated_at
	`, c.ID, c.Title, noSaveInt, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) GetConversation(id string) (*ConversationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var c ConversationRecord
	var noSaveInt int
	err := s.db.QueryRow(`
		SELECT id, title, no_save, created_at, updated_at
		FROM conversations WHERE id = ?
	`, id).Scan(&c.ID, &c.Title, &noSaveInt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.NoSave = noSaveInt != 0
	return &c, nil
}

func (s *Store) ListConversations() ([]ConversationRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, title, no_save, created_at, updated_at FROM conversations ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []ConversationRecord
	for rows.Next() {
		var c ConversationRecord
		var noSaveInt int
		if err := rows.Scan(&c.ID, &c.Title, &noSaveInt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.NoSave = noSaveInt != 0
		convs = append(convs, c)
	}
	return convs, rows.Err()
}

func (s *Store) DeleteConversation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM conversations WHERE id = ?", id)
	return err
}

type MessageRecord struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	HostMemberID   string
	ModelID        string
	CreatedAt      int64
	// ParentID is the message this one follows. Empty means the root of the
	// conversation. Messages sharing a parent are alternative takes on the
	// same turn -- a regenerated answer, or a re-asked question.
	ParentID string
	// Active marks the sibling currently on the visible branch. Exactly one
	// child per parent is active.
	Active bool
}

// SaveMessage inserts a message and makes it the active child of its parent,
// so a freshly generated alternative is the one shown. The switch happens in
// the same transaction as the insert: a parent with two active children would
// make the transcript ambiguous.
func (s *Store) SaveMessage(m MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		UPDATE messages SET active = 0 WHERE conversation_id = ? AND parent_id = ?
	`, m.ConversationID, m.ParentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO messages (id, conversation_id, role, content, host_member_id, model_id, created_at, parent_id, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
	`, m.ID, m.ConversationID, m.Role, m.Content, m.HostMemberID, m.ModelID, m.CreatedAt, m.ParentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ActivateMessage puts a message on the visible branch in place of whichever
// sibling currently holds the slot.
func (s *Store) ActivateMessage(conversationID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var parentID string
	if err := tx.QueryRow(`
		SELECT parent_id FROM messages WHERE id = ? AND conversation_id = ?
	`, messageID, conversationID).Scan(&parentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE messages SET active = 0 WHERE conversation_id = ? AND parent_id = ?
	`, conversationID, parentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE messages SET active = 1 WHERE id = ? AND conversation_id = ?
	`, messageID, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMessages returns every message in the conversation, including the
// branches that are not currently visible. Callers that want the transcript
// as read should walk the active chain.
func (s *Store) ListMessages(conversationID string) ([]MessageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, conversation_id, role, content, host_member_id, model_id, created_at, parent_id, active
		FROM messages WHERE conversation_id = ? ORDER BY created_at ASC, rowid ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []MessageRecord
	for rows.Next() {
		var m MessageRecord
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.HostMemberID, &m.ModelID, &m.CreatedAt, &m.ParentID, &m.Active); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Hosted Models
type HostedModelRecord struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ModelType    string `json:"model_type"`
	EndpointURL  string `json:"endpoint_url"`
	APIKey       string `json:"api_key,omitempty"`
	ContextLimit int    `json:"context_limit"`
	MaxTokens    int    `json:"max_tokens"`
	Enabled      bool   `json:"enabled"`
	Published    bool   `json:"published"`
	Revision     int    `json:"revision"`
	// BackendModel is the identifier the backend server expects in the
	// OpenAI "model" field. It is separate from ID (opaque, room-wide) and
	// Name (owner-facing label) so a display name can be changed without
	// breaking dispatch.
	BackendModel string `json:"backend_model"`
	// AllowPrivateNetwork opts this one endpoint out of the private-range
	// SSRF block. It is per-model and off by default.
	AllowPrivateNetwork bool     `json:"allow_private_network"`
	Filename            string   `json:"filename,omitempty"`
	Threads             int      `json:"threads,omitempty"`
	GPULayers           *int     `json:"gpu_layers,omitempty"`
	Projector           string   `json:"projector,omitempty"`
	DraftModel          string   `json:"draft_model,omitempty"`
	ExtraArgs           []string `json:"extra_args,omitempty"`
	// SupportsThinking reports that this model can be asked to reason, so a
	// requester may choose how hard it should think.
	SupportsThinking bool `json:"supports_thinking,omitempty"`
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func encodeStringSlice(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeStringSlice(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

func (s *Store) SaveHostedModel(m HostedModelRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	enabledInt := 0
	if m.Enabled {
		enabledInt = 1
	}
	publishedInt := 0
	if m.Published {
		publishedInt = 1
	}

	allowPrivateInt := 0
	if m.AllowPrivateNetwork {
		allowPrivateInt = 1
	}
	thinkingInt := 0
	if m.SupportsThinking {
		thinkingInt = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO hosted_models (
			id, name, model_type, endpoint_url, api_key, context_limit,
			max_tokens, enabled, published, revision, backend_model,
			allow_private_network, filename, threads, gpu_layers, projector,
			draft_model, extra_args, supports_thinking
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			model_type = excluded.model_type,
			endpoint_url = excluded.endpoint_url,
			api_key = excluded.api_key,
			context_limit = excluded.context_limit,
			max_tokens = excluded.max_tokens,
			enabled = excluded.enabled,
			published = excluded.published,
			revision = excluded.revision,
			backend_model = excluded.backend_model,
			allow_private_network = excluded.allow_private_network,
			filename = excluded.filename,
			threads = excluded.threads,
			gpu_layers = excluded.gpu_layers,
			projector = excluded.projector,
			draft_model = excluded.draft_model,
			extra_args = excluded.extra_args,
			supports_thinking = excluded.supports_thinking
	`, m.ID, m.Name, m.ModelType, m.EndpointURL, m.APIKey, m.ContextLimit,
		m.MaxTokens, enabledInt, publishedInt, m.Revision, m.BackendModel,
		allowPrivateInt, m.Filename, m.Threads, nullableInt(m.GPULayers), m.Projector,
		m.DraftModel, encodeStringSlice(m.ExtraArgs), thinkingInt)
	return err
}

func (s *Store) GetHostedModel(id string) (*HostedModelRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var m HostedModelRecord
	var enabledInt, publishedInt, allowPrivateInt, thinkingInt int
	var gpuLayers sql.NullInt64
	var extraArgs string
	err := s.db.QueryRow(`
		SELECT id, name, model_type, endpoint_url, api_key, context_limit,
		       max_tokens, enabled, published, revision, backend_model,
		       allow_private_network, filename, threads, gpu_layers, projector,
		       draft_model, extra_args, supports_thinking
		FROM hosted_models WHERE id = ?
	`, id).Scan(
		&m.ID, &m.Name, &m.ModelType, &m.EndpointURL, &m.APIKey, &m.ContextLimit,
		&m.MaxTokens, &enabledInt, &publishedInt, &m.Revision, &m.BackendModel,
		&allowPrivateInt, &m.Filename, &m.Threads, &gpuLayers, &m.Projector,
		&m.DraftModel, &extraArgs, &thinkingInt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Enabled = enabledInt != 0
	m.Published = publishedInt != 0
	m.AllowPrivateNetwork = allowPrivateInt != 0
	m.SupportsThinking = thinkingInt != 0
	if gpuLayers.Valid {
		v := int(gpuLayers.Int64)
		m.GPULayers = &v
	}
	m.ExtraArgs = decodeStringSlice(extraArgs)
	return &m, nil
}

func (s *Store) ListHostedModels() ([]HostedModelRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, name, model_type, endpoint_url, api_key, context_limit,
		       max_tokens, enabled, published, revision, backend_model,
		       allow_private_network, filename, threads, gpu_layers, projector,
		       draft_model, extra_args, supports_thinking
		FROM hosted_models ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []HostedModelRecord
	for rows.Next() {
		var m HostedModelRecord
		var enabledInt, publishedInt, allowPrivateInt, thinkingInt int
		var gpuLayers sql.NullInt64
		var extraArgs string
		if err := rows.Scan(
			&m.ID, &m.Name, &m.ModelType, &m.EndpointURL, &m.APIKey, &m.ContextLimit,
			&m.MaxTokens, &enabledInt, &publishedInt, &m.Revision, &m.BackendModel,
			&allowPrivateInt, &m.Filename, &m.Threads, &gpuLayers, &m.Projector,
			&m.DraftModel, &extraArgs, &thinkingInt,
		); err != nil {
			return nil, err
		}
		m.Enabled = enabledInt != 0
		m.Published = publishedInt != 0
		m.AllowPrivateNetwork = allowPrivateInt != 0
		m.SupportsThinking = thinkingInt != 0
		if gpuLayers.Valid {
			v := int(gpuLayers.Int64)
			m.GPULayers = &v
		}
		m.ExtraArgs = decodeStringSlice(extraArgs)
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *Store) DeleteHostedModel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM hosted_models WHERE id = ?", id)
	return err
}

// Host Limits
type HostLimitsRecord struct {
	MaxActive               int
	MaxQueuedPerMember      int
	MaxQueuedTotal          int
	QueueTimeoutSeconds     int
	ExecutionTimeoutSeconds int
	// IdleUnloadSeconds is how long a demand-loaded managed model stays warm
	// once the queue drains. Negative never releases it, zero releases it
	// straight away, and a positive value waits that many seconds.
	IdleUnloadSeconds int
}

// DefaultIdleUnloadSeconds keeps an engine warm for five minutes, which is
// what the runner did before the owner could choose.
const DefaultIdleUnloadSeconds = 300

func (s *Store) GetHostLimits() (HostLimitsRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var l HostLimitsRecord
	err := s.db.QueryRow(`
		SELECT max_active, max_queued_per_member, max_queued_total,
		       queue_timeout_seconds, execution_timeout_seconds, idle_unload_seconds
		FROM host_limits WHERE id = 1
	`).Scan(&l.MaxActive, &l.MaxQueuedPerMember, &l.MaxQueuedTotal, &l.QueueTimeoutSeconds,
		&l.ExecutionTimeoutSeconds, &l.IdleUnloadSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return HostLimitsRecord{
			MaxActive:               1,
			MaxQueuedPerMember:      1,
			MaxQueuedTotal:          10,
			QueueTimeoutSeconds:     300,
			ExecutionTimeoutSeconds: 600,
			IdleUnloadSeconds:       DefaultIdleUnloadSeconds,
		}, nil
	}
	return l, err
}

func (s *Store) SaveHostLimits(l HostLimitsRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO host_limits (
			id, max_active, max_queued_per_member, max_queued_total,
			queue_timeout_seconds, execution_timeout_seconds, idle_unload_seconds
		) VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			max_active = excluded.max_active,
			max_queued_per_member = excluded.max_queued_per_member,
			max_queued_total = excluded.max_queued_total,
			queue_timeout_seconds = excluded.queue_timeout_seconds,
			execution_timeout_seconds = excluded.execution_timeout_seconds,
			idle_unload_seconds = excluded.idle_unload_seconds
	`, l.MaxActive, l.MaxQueuedPerMember, l.MaxQueuedTotal, l.QueueTimeoutSeconds,
		l.ExecutionTimeoutSeconds, l.IdleUnloadSeconds)
	return err
}

// Community
type ChannelRecord struct {
	ID          string `json:"id"`
	RoomID      string `json:"room_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   int64  `json:"created_at"`
	// CreatedBy is the member who announced this channel. It is who, besides
	// the room's creator, is allowed to withdraw it.
	CreatedBy string `json:"created_by,omitempty"`
}

func (s *Store) SaveChannel(c ChannelRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}

	_, err := s.db.Exec(`
		INSERT INTO community_channels (id, room_id, name, description, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			-- A row written before the author was recorded learns it from the
			-- log on the next pass, but an empty value never overwrites one.
			created_by = CASE WHEN excluded.created_by = '' THEN created_by ELSE excluded.created_by END
	`, c.ID, c.RoomID, c.Name, c.Description, c.CreatedAt, c.CreatedBy)
	return err
}

// DeleteChannel removes a channel's local row. The event log remains the
// record of what happened; this is the materialized view catching up with it.
func (s *Store) DeleteChannel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM community_channels WHERE id = ?", id)
	return err
}

func (s *Store) ListChannels(roomID string) ([]ChannelRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, room_id, name, description, created_at, created_by FROM community_channels WHERE room_id = ? ORDER BY created_at ASC", roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ChannelRecord
	for rows.Next() {
		var c ChannelRecord
		if err := rows.Scan(&c.ID, &c.RoomID, &c.Name, &c.Description, &c.CreatedAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

type EventRecord struct {
	ID               string `json:"id"`
	RoomID           string `json:"room_id"`
	ChannelID        string `json:"channel_id"`
	AuthorMemberID   string `json:"author_member_id"`
	AuthorSeq        int64  `json:"author_seq"`
	EventType        string `json:"event_type"` // message, edit, delete, tombstone
	TargetEventID    string `json:"target_event_id,omitempty"`
	Content          string `json:"content"`
	Timestamp        int64  `json:"timestamp"`
	Signature        string `json:"signature"`
	ReplicatedStatus string `json:"replicated_status"` // local, pending, replicated
	SigVersion       uint8  `json:"sig_version"`
	CreatedAt        int64  `json:"created_at"`
}

func (s *Store) SaveEvent(e EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	if e.ReplicatedStatus == "" {
		e.ReplicatedStatus = "local"
	}

	_, err := s.db.Exec(`
		INSERT INTO community_events (
			id, room_id, channel_id, author_member_id, author_seq,
			event_type, target_event_id, content, timestamp, signature,
			replicated_status, created_at, sig_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			replicated_status = excluded.replicated_status
	`, e.ID, e.RoomID, e.ChannelID, e.AuthorMemberID, e.AuthorSeq,
		e.EventType, e.TargetEventID, e.Content, e.Timestamp, e.Signature,
		e.ReplicatedStatus, e.CreatedAt, e.SigVersion)
	return err
}

func (s *Store) GetLatestAuthorSeq(roomID, authorMemberID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var maxSeq sql.NullInt64
	err := s.db.QueryRow(`
		SELECT MAX(author_seq) FROM community_events
		WHERE room_id = ? AND author_member_id = ?
	`, roomID, authorMemberID).Scan(&maxSeq)
	if err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 0, nil
	}
	return maxSeq.Int64, nil
}

func (s *Store) ListEvents(channelID string) ([]EventRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, room_id, channel_id, author_member_id, author_seq,
		       event_type, COALESCE(target_event_id, ''), content, timestamp, signature,
		       replicated_status, created_at, sig_version
		FROM community_events
		WHERE channel_id = ?
		ORDER BY timestamp ASC, author_seq ASC
	`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []EventRecord
	for rows.Next() {
		var e EventRecord
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.ChannelID, &e.AuthorMemberID, &e.AuthorSeq,
			&e.EventType, &e.TargetEventID, &e.Content, &e.Timestamp, &e.Signature,
			&e.ReplicatedStatus, &e.CreatedAt, &e.SigVersion,
		); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) GetEvent(id string) (*EventRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var e EventRecord
	err := s.db.QueryRow(`
		SELECT id, room_id, channel_id, author_member_id, author_seq,
		       event_type, COALESCE(target_event_id, ''), content, timestamp, signature,
		       replicated_status, created_at, sig_version
		FROM community_events
		WHERE id = ?
	`, id).Scan(
		&e.ID, &e.RoomID, &e.ChannelID, &e.AuthorMemberID, &e.AuthorSeq,
		&e.EventType, &e.TargetEventID, &e.Content, &e.Timestamp, &e.Signature,
		&e.ReplicatedStatus, &e.CreatedAt, &e.SigVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) ListAllEvents(roomID string) ([]EventRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, room_id, channel_id, author_member_id, author_seq,
		       event_type, COALESCE(target_event_id, ''), content, timestamp, signature,
		       replicated_status, created_at, sig_version
		FROM community_events
		WHERE room_id = ?
		ORDER BY timestamp ASC, author_seq ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []EventRecord
	for rows.Next() {
		var e EventRecord
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.ChannelID, &e.AuthorMemberID, &e.AuthorSeq,
			&e.EventType, &e.TargetEventID, &e.Content, &e.Timestamp, &e.Signature,
			&e.ReplicatedStatus, &e.CreatedAt, &e.SigVersion,
		); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) PurgeExpiredEvents(roomID string, olderThanTimestamp int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only purge events that are not tombstones (tombstones are preserved for the sync horizon)
	res, err := s.db.Exec(`
		DELETE FROM community_events
		WHERE room_id = ? AND timestamp < ? AND event_type != 'tombstone'
	`, roomID, olderThanTimestamp)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) SaveReadState(channelID string, lastRead int64, muted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	mutedInt := 0
	if muted {
		mutedInt = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO community_read_state (channel_id, last_read_timestamp, muted)
		VALUES (?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			last_read_timestamp = excluded.last_read_timestamp,
			muted = excluded.muted
	`, channelID, lastRead, mutedInt)
	return err
}

func (s *Store) GetReadState(channelID string) (int64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lastRead int64
	var mutedInt int
	err := s.db.QueryRow(`
		SELECT last_read_timestamp, muted FROM community_read_state WHERE channel_id = ?
	`, channelID).Scan(&lastRead, &mutedInt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return lastRead, mutedInt != 0, err
}

// Contribution Receipts (Social recognition)
type ContributionReceiptRecord struct {
	RequestID          string
	RoomID             string
	HostMemberID       string
	RequesterMemberID  string
	Timestamp          int64
	Completed          bool
	HostSignature      string
	RequesterSignature string
	ReplicatedStatus   string
	SigVersion         uint8
	CreatedAt          int64
}

func (s *Store) SaveReceipt(r ContributionReceiptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.ReplicatedStatus == "" {
		r.ReplicatedStatus = "local"
	}

	completedInt := 0
	if r.Completed {
		completedInt = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO contribution_receipts (
			request_id, room_id, host_member_id, requester_member_id,
			timestamp, completed, host_signature, requester_signature,
			replicated_status, created_at, sig_version
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id) DO UPDATE SET
			replicated_status = excluded.replicated_status,
			requester_signature = excluded.requester_signature,
			sig_version = excluded.sig_version
	`, r.RequestID, r.RoomID, r.HostMemberID, r.RequesterMemberID,
		r.Timestamp, completedInt, r.HostSignature, r.RequesterSignature,
		r.ReplicatedStatus, r.CreatedAt, r.SigVersion)
	return err
}

func (s *Store) GetReceipt(requestID string) (*ContributionReceiptRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var r ContributionReceiptRecord
	var completedInt int
	err := s.db.QueryRow(`
		SELECT request_id, room_id, host_member_id, requester_member_id,
		       timestamp, completed, host_signature, requester_signature,
		       replicated_status, created_at, sig_version
		FROM contribution_receipts WHERE request_id = ?
	`, requestID).Scan(
		&r.RequestID, &r.RoomID, &r.HostMemberID, &r.RequesterMemberID,
		&r.Timestamp, &completedInt, &r.HostSignature, &r.RequesterSignature,
		&r.ReplicatedStatus, &r.CreatedAt, &r.SigVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Completed = completedInt != 0
	return &r, nil
}

func (s *Store) ListReceipts(roomID string) ([]ContributionReceiptRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT request_id, room_id, host_member_id, requester_member_id,
		       timestamp, completed, host_signature, requester_signature,
		       replicated_status, created_at, sig_version
		FROM contribution_receipts WHERE room_id = ?
		ORDER BY timestamp ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var receipts []ContributionReceiptRecord
	for rows.Next() {
		var r ContributionReceiptRecord
		var completedInt int
		if err := rows.Scan(
			&r.RequestID, &r.RoomID, &r.HostMemberID, &r.RequesterMemberID,
			&r.Timestamp, &completedInt, &r.HostSignature, &r.RequesterSignature,
			&r.ReplicatedStatus, &r.CreatedAt, &r.SigVersion); err != nil {
			return nil, err
		}
		r.Completed = completedInt != 0
		receipts = append(receipts, r)
	}
	return receipts, rows.Err()
}
