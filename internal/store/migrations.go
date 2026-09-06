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
}
