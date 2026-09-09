package store

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS local_settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS device_identity (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		tailcat_key TEXT NOT NULL,
		device_private BLOB NOT NULL,
		device_cert_der BLOB NOT NULL,
		device_public TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS room_state (
		room_id TEXT PRIMARY KEY,
		role TEXT NOT NULL CHECK (role IN ('creator', 'member')),
		room_name TEXT NOT NULL,
		authority_public TEXT NOT NULL,
		authority_private BLOB,
		bootstrap_addr TEXT NOT NULL,
		invitation_code TEXT,
		invitation_id TEXT,
		admission_secret_hash TEXT,
		approval_mode INTEGER NOT NULL DEFAULT 0,
		roster_version INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS members (
		member_id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		device_public TEXT NOT NULL,
		display_name TEXT NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('admitted', 'pending', 'removed')),
		roster_version INTEGER NOT NULL,
		signature TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS peer_addresses (
		member_id TEXT PRIMARY KEY,
		tailcat_addr TEXT NOT NULL,
		last_seen INTEGER NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS conversations (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		no_save INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK (role IN ('system', 'user', 'assistant')),
		content TEXT NOT NULL,
		host_member_id TEXT,
		model_id TEXT,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS hosted_models (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		model_type TEXT NOT NULL CHECK (model_type IN ('external', 'managed')),
		endpoint_url TEXT NOT NULL,
		api_key TEXT,
		context_limit INTEGER NOT NULL DEFAULT 4096,
		max_tokens INTEGER NOT NULL DEFAULT 1024,
		enabled INTEGER NOT NULL DEFAULT 1,
		published INTEGER NOT NULL DEFAULT 1,
		revision INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS host_limits (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		max_active INTEGER NOT NULL DEFAULT 1,
		max_queued_per_member INTEGER NOT NULL DEFAULT 1,
		max_queued_total INTEGER NOT NULL DEFAULT 10,
		queue_timeout_seconds INTEGER NOT NULL DEFAULT 300,
		execution_timeout_seconds INTEGER NOT NULL DEFAULT 600
	);`,

	`CREATE TABLE IF NOT EXISTS community_channels (
		id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS community_events (
		id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		author_member_id TEXT NOT NULL,
		author_seq INTEGER NOT NULL,
		event_type TEXT NOT NULL CHECK (event_type IN ('message', 'edit', 'delete', 'tombstone')),
		target_event_id TEXT,
		content TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		signature TEXT NOT NULL,
		replicated_status TEXT NOT NULL DEFAULT 'local' CHECK (replicated_status IN ('local', 'pending', 'replicated')),
		created_at INTEGER NOT NULL,
		UNIQUE(room_id, author_member_id, author_seq)
	);

	CREATE INDEX IF NOT EXISTS idx_community_events_channel ON community_events(channel_id, timestamp);

	CREATE TABLE IF NOT EXISTS community_read_state (
		channel_id TEXT PRIMARY KEY,
		last_read_timestamp INTEGER NOT NULL,
		muted INTEGER NOT NULL DEFAULT 0
	);`,

	// Migration 004: Contribution receipts and social recognition
	`CREATE TABLE IF NOT EXISTS contribution_receipts (
		request_id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		host_member_id TEXT NOT NULL,
		requester_member_id TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		completed INTEGER NOT NULL DEFAULT 1,
		host_signature TEXT NOT NULL,
		requester_signature TEXT NOT NULL,
		replicated_status TEXT NOT NULL DEFAULT 'local' CHECK (replicated_status IN ('local', 'replicated')),
		created_at INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_contrib_room_time ON contribution_receipts(room_id, timestamp);
	CREATE INDEX IF NOT EXISTS idx_contrib_pair_time ON contribution_receipts(host_member_id, requester_member_id, timestamp);`,

	// Migration 005: stable transport identity, authority-signed bootstrap
	// certificate, distinct backend model names, and per-author event
	// divergence that is quarantined rather than silently dropped.
	`ALTER TABLE device_identity ADD COLUMN tailcat_psk TEXT NOT NULL DEFAULT '';
	ALTER TABLE device_identity ADD COLUMN tailcat_addr TEXT NOT NULL DEFAULT '';

	ALTER TABLE room_state ADD COLUMN room_cert_der BLOB;

	ALTER TABLE hosted_models ADD COLUMN backend_model TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosted_models ADD COLUMN allow_private_network INTEGER NOT NULL DEFAULT 0;

	CREATE INDEX IF NOT EXISTS idx_members_device_public ON members(device_public);

	-- sig_version records which signing payload format each stored signature
	-- was made over, so canonicalizing payloads does not invalidate records
	-- written before the change.
	ALTER TABLE members ADD COLUMN sig_version INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE contribution_receipts ADD COLUMN sig_version INTEGER NOT NULL DEFAULT 0;

	-- community_events is rebuilt to (a) admit the replicated 'channel' event
	-- type and (b) drop UNIQUE(room_id, author_member_id, author_seq), which
	-- silently kept whichever conflicting event arrived first and let peers
	-- diverge. Conflicts are now stored and quarantined by the materializer.
	CREATE TABLE community_events_v2 (
		id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		author_member_id TEXT NOT NULL,
		author_seq INTEGER NOT NULL,
		event_type TEXT NOT NULL CHECK (event_type IN ('message', 'edit', 'delete', 'tombstone', 'channel')),
		target_event_id TEXT,
		content TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		signature TEXT NOT NULL,
		replicated_status TEXT NOT NULL DEFAULT 'local' CHECK (replicated_status IN ('local', 'pending', 'replicated')),
		created_at INTEGER NOT NULL,
		sig_version INTEGER NOT NULL DEFAULT 0
	);

	INSERT INTO community_events_v2 (
		id, room_id, channel_id, author_member_id, author_seq, event_type,
		target_event_id, content, timestamp, signature, replicated_status, created_at
	)
		SELECT id, room_id, channel_id, author_member_id, author_seq, event_type,
		       target_event_id, content, timestamp, signature, replicated_status, created_at
		FROM community_events;

	DROP TABLE community_events;
	ALTER TABLE community_events_v2 RENAME TO community_events;

	CREATE INDEX IF NOT EXISTS idx_community_events_channel ON community_events(channel_id, timestamp);
	CREATE INDEX IF NOT EXISTS idx_community_events_author ON community_events(room_id, author_member_id, author_seq);`,

	// Migration 006: a conversation becomes a tree instead of a list, so a
	// message can be regenerated or edited without destroying the answer it
	// replaces. Children of the same parent are alternative takes on the same
	// turn; exactly one of them is active, and the active chain from the root
	// is the transcript the reader sees and the model is sent.
	`ALTER TABLE messages ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE messages ADD COLUMN active INTEGER NOT NULL DEFAULT 1;

	-- Existing transcripts are linear, so each message's parent is simply the
	-- one before it. created_at has one-second resolution and a fast exchange
	-- can share a timestamp, so rowid breaks ties in insertion order.
	UPDATE messages SET parent_id = COALESCE((
		SELECT prev.id FROM messages prev
		WHERE prev.conversation_id = messages.conversation_id
		  AND (prev.created_at < messages.created_at
		       OR (prev.created_at = messages.created_at AND prev.rowid < messages.rowid))
		ORDER BY prev.created_at DESC, prev.rowid DESC
		LIMIT 1
	), '');

	CREATE INDEX IF NOT EXISTS idx_messages_conv_parent ON messages(conversation_id, parent_id);`,

	// Migration 007: managed model load configuration. The runner may be
	// unloaded between requests (or restarted), so the complete load request
	// belongs with the hosted-model record rather than in process memory.
	`ALTER TABLE hosted_models ADD COLUMN filename TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosted_models ADD COLUMN threads INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE hosted_models ADD COLUMN gpu_layers INTEGER;
	ALTER TABLE hosted_models ADD COLUMN projector TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosted_models ADD COLUMN draft_model TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosted_models ADD COLUMN extra_args TEXT NOT NULL DEFAULT '';`,

	// Migration 008: how long a demand-loaded engine stays warm. Negative
	// keeps it loaded until an explicit unload, zero releases it as soon as
	// the queue drains, and the default preserves the five minutes that were
	// hardcoded before the owner could choose.
	`ALTER TABLE host_limits ADD COLUMN idle_unload_seconds INTEGER NOT NULL DEFAULT 300;`,

	// Migration 009: whether a managed model's own chat template has a
	// reasoning path. It decides whether a requester is offered a thinking
	// level at all, so it travels with the model rather than being re-derived
	// from the weights on every catalog read.
	`ALTER TABLE hosted_models ADD COLUMN supports_thinking INTEGER NOT NULL DEFAULT 0;`,

	// Migration 010: a channel can be deleted. The event log is the record of
	// what the room has, so a withdrawal has to be an event like the
	// announcement it undoes, and community_events must admit its type. The
	// channel's author is recorded alongside it so the local rows can answer
	// who may delete one without replaying the log first.
	`ALTER TABLE community_channels ADD COLUMN created_by TEXT NOT NULL DEFAULT '';

	CREATE TABLE community_events_v3 (
		id TEXT PRIMARY KEY,
		room_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		author_member_id TEXT NOT NULL,
		author_seq INTEGER NOT NULL,
		event_type TEXT NOT NULL CHECK (event_type IN ('message', 'edit', 'delete', 'tombstone', 'channel', 'channel_delete')),
		target_event_id TEXT,
		content TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		signature TEXT NOT NULL,
		replicated_status TEXT NOT NULL DEFAULT 'local' CHECK (replicated_status IN ('local', 'pending', 'replicated')),
		created_at INTEGER NOT NULL,
		sig_version INTEGER NOT NULL DEFAULT 0
	);

	INSERT INTO community_events_v3 (
		id, room_id, channel_id, author_member_id, author_seq, event_type,
		target_event_id, content, timestamp, signature, replicated_status,
		created_at, sig_version
	)
		SELECT id, room_id, channel_id, author_member_id, author_seq, event_type,
		       target_event_id, content, timestamp, signature, replicated_status,
		       created_at, sig_version
		FROM community_events;

	DROP TABLE community_events;
	ALTER TABLE community_events_v3 RENAME TO community_events;

	CREATE INDEX IF NOT EXISTS idx_community_events_channel ON community_events(channel_id, timestamp);
	CREATE INDEX IF NOT EXISTS idx_community_events_author ON community_events(room_id, author_member_id, author_seq);`,
}
