package store

import (
	"path/filepath"
	"testing"
)

func TestStoreMigrationsAndOperations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "woolwire.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// Settings
	if err := s.SetSetting("display_name", "Alice"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	val, err := s.GetSetting("display_name")
	if err != nil || val != "Alice" {
		t.Fatalf("get setting: got %q, err %v", val, err)
	}

	// Device identity
	ident := DeviceIdentity{
		TailcatKey:    "node-key-1",
		DevicePrivate: []byte("priv-key"),
		DeviceCertDER: []byte("cert-der"),
		DevicePublic:  "pub-token",
	}
	if err := s.SaveDeviceIdentity(ident); err != nil {
		t.Fatalf("save device identity: %v", err)
	}
	gotIdent, err := s.GetDeviceIdentity()
	if err != nil || gotIdent.DevicePublic != "pub-token" {
		t.Fatalf("get device identity: got %#v, err %v", gotIdent, err)
	}

	// Room state
	room := RoomRecord{
		RoomID:              "room-123",
		Role:                "creator",
		RoomName:            "Test Room",
		AuthorityPublic:     "auth-pub",
		AuthorityPrivate:    []byte("auth-priv"),
		BootstrapAddr:       "tailcat-addr",
		InvitationCode:      "invite-code",
		InvitationID:        "invite-id",
		AdmissionSecretHash: "secret-hash",
		ApprovalMode:        false,
		RosterVersion:       1,
	}
	if err := s.SaveRoomState(room); err != nil {
		t.Fatalf("save room state: %v", err)
	}
	gotRoom, err := s.GetRoomState()
	if err != nil || gotRoom.RoomName != "Test Room" || gotRoom.Role != "creator" {
		t.Fatalf("get room state: got %#v, err %v", gotRoom, err)
	}

	// Members
	m1 := MemberRecord{
		MemberID:      "member-a",
		RoomID:        "room-123",
		DevicePublic:  "pub-a",
		DisplayName:   "Alice",
		Status:        "admitted",
		RosterVersion: 1,
		Signature:     "sig-a",
	}
	if err := s.SaveMember(m1); err != nil {
		t.Fatalf("save member: %v", err)
	}

	members, err := s.ListMembers("room-123")
	if err != nil || len(members) != 1 || members[0].DisplayName != "Alice" {
		t.Fatalf("list members: got %#v, err %v", members, err)
	}

	// Peer address
	if err := s.SavePeerAddress("member-a", "tailcat-member-a"); err != nil {
		t.Fatalf("save peer address: %v", err)
	}
	peers, err := s.ListPeerAddresses()
	if err != nil || len(peers) != 1 || peers[0].TailcatAddr != "tailcat-member-a" {
		t.Fatalf("list peers: got %#v, err %v", peers, err)
	}

	// Reopen database to verify persistence on disk
	s.Close()
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	val2, err := reopened.GetSetting("display_name")
	if err != nil || val2 != "Alice" {
		t.Fatalf("reopened get setting: got %q, err %v", val2, err)
	}

	// Test DatabaseSize
	size := reopened.DatabaseSize()
	if size <= 0 {
		t.Fatalf("expected positive database size, got %d", size)
	}

	// Test Backup
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := reopened.Backup(backupPath); err != nil {
		t.Fatalf("backup failed: %v", err)
	}

	// Verify backup can be opened independently
	backupStore, err := Open(backupPath)
	if err != nil {
		t.Fatalf("open backup store failed: %v", err)
	}
	defer backupStore.Close()

	backupVal, err := backupStore.GetSetting("display_name")
	if err != nil || backupVal != "Alice" {
		t.Fatalf("backup store setting got %q, err %v", backupVal, err)
	}
}
