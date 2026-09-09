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

// TestMessageVariantsShareAParent covers the tree behind regenerate: a second
// answer for the same turn must supersede the first without deleting it.
func TestMessageVariantsShareAParent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "woolwire.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if err := s.SaveConversation(ConversationRecord{ID: "c1", Title: "Chat"}); err != nil {
		t.Fatalf("save conversation: %v", err)
	}
	save := func(id, parent, role string) {
		t.Helper()
		if err := s.SaveMessage(MessageRecord{
			ID: id, ConversationID: "c1", Role: role, Content: id, ParentID: parent,
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("u1", "", "user")
	save("a1", "u1", "assistant")
	save("a2", "u1", "assistant") // a regenerated answer

	msgs, err := s.ListMessages("c1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want the superseded answer kept", len(msgs))
	}
	active := map[string]bool{}
	for _, m := range msgs {
		active[m.ID] = m.Active
	}
	if active["a1"] || !active["a2"] {
		t.Errorf("newest answer should be active: a1=%v a2=%v", active["a1"], active["a2"])
	}

	// Paging back to the earlier take moves the flag, and only the flag.
	if err := s.ActivateMessage("c1", "a1"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	msgs, _ = s.ListMessages("c1")
	for _, m := range msgs {
		if m.ID == "a1" && !m.Active {
			t.Error("a1 should be active after switching to it")
		}
		if m.ID == "a2" && m.Active {
			t.Error("a2 should be inactive after switching away")
		}
		if m.ID == "u1" && !m.Active {
			t.Error("switching an answer must not deactivate the question")
		}
	}
}

func TestManagedLoadConfigurationSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "woolwire.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	layers := 17
	expected := HostedModelRecord{
		ID: "managed-1", Name: "Vision", ModelType: "managed", EndpointURL: "runner://managed",
		BackendModel: "Vision", ContextLimit: 8192, MaxTokens: 2048, Enabled: true, Published: true,
		Revision: 3, Filename: "hub/models--org--vision/snapshots/r1/model.gguf", Threads: 8,
		GPULayers: &layers, Projector: "hub/models--org--vision/mmproj.gguf", DraftModel: "draft.gguf",
		ExtraArgs: []string{"--flash-attn=on", "--batch-size=512"},
	}
	if err := s.SaveHostedModel(expected); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetHostedModel(expected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != expected.Filename || got.Threads != expected.Threads || got.Projector != expected.Projector || got.DraftModel != expected.DraftModel {
		t.Fatalf("managed load paths did not survive restart: %#v", got)
	}
	if got.GPULayers == nil || *got.GPULayers != layers || len(got.ExtraArgs) != 2 || got.ExtraArgs[1] != expected.ExtraArgs[1] {
		t.Fatalf("managed load tuning did not survive restart: %#v", got)
	}
}
