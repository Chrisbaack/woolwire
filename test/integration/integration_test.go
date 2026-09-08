//go:build integration

// Package integration drives real cmd/woolwire processes over their local
// HTTP APIs. It needs outbound network access: the nodes talk to each other
// over real Tailcat, which reaches DERP relays and tailcat.dev.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	startupTimeout = 3 * time.Minute
	requestTimeout = 2 * time.Minute
)

var setupSecretPattern = regexp.MustCompile(`Setup Secret \(one-time, shown only now\): (\S+)`)

// node is one woolwire process under test.
type node struct {
	t        *testing.T
	name     string
	binary   string
	stateDir string
	listen   string
	peerPort int

	mu     sync.Mutex
	cmd    *exec.Cmd
	output *syncBuffer

	secret  string
	cookie  *http.Cookie
	client  *http.Client
	baseURL string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func buildBinary(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "woolwire")
	cmd := exec.Command("go", "build", "-o", binary, "github.com/Chrisbaack/woolwire/cmd/woolwire")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build woolwire: %v", err)
	}
	return binary
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newNode(t *testing.T, binary, name string) *node {
	t.Helper()

	n := &node{
		t:        t,
		name:     name,
		binary:   binary,
		stateDir: t.TempDir(),
		listen:   fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		peerPort: 4242,
		client:   &http.Client{Timeout: requestTimeout},
	}
	n.baseURL = "http://" + n.listen
	t.Cleanup(n.stop)
	return n
}

// start launches the process and waits for its local API to answer.
func (n *node) start() {
	n.t.Helper()

	out := &syncBuffer{}
	cmd := exec.Command(n.binary,
		"-listen", n.listen,
		"-state", n.stateDir,
		"-models", filepath.Join(n.stateDir, "models"),
		"-peer-port", fmt.Sprintf("%d", n.peerPort),
	)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		n.t.Fatalf("%s: start: %v", n.name, err)
	}

	n.mu.Lock()
	n.cmd = cmd
	n.output = out
	n.mu.Unlock()

	deadline := time.Now().Add(startupTimeout)
	for {
		if resp, err := n.client.Get(n.baseURL + "/api/v1/state"); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s did not start within %s:\n%s", n.name, startupTimeout, out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The one-time secret is printed only on the boot that mints it.
	if n.secret == "" {
		if m := setupSecretPattern.FindStringSubmatch(out.String()); len(m) == 2 {
			n.secret = m[1]
		} else {
			n.t.Fatalf("%s printed no setup secret:\n%s", n.name, out.String())
		}
	}
}

func (n *node) stop() {
	n.mu.Lock()
	cmd := n.cmd
	n.cmd = nil
	n.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)

	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// restart stops and starts the process against the same state directory.
func (n *node) restart() {
	n.stop()
	time.Sleep(time.Second)
	n.start()
}

func (n *node) do(method, path string, payload any) (*http.Response, []byte) {
	n.t.Helper()

	var body io.Reader = bytes.NewReader(nil)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			n.t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, n.baseURL+path, body)
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Woolwire-Request", "1")
	if n.cookie != nil {
		req.AddCookie(n.cookie)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (n *node) mustDo(method, path string, payload any, out any) {
	n.t.Helper()

	resp, raw := n.do(method, path, payload)
	if resp.StatusCode != http.StatusOK {
		n.t.Fatalf("%s %s on %s returned %d: %s", method, path, n.name, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			n.t.Fatalf("decode %s response: %v (%s)", path, err, raw)
		}
	}
}

func (n *node) login() {
	n.t.Helper()

	resp, raw := n.do("POST", "/api/v1/setup", map[string]string{"token": n.secret})
	if resp.StatusCode != http.StatusOK {
		n.t.Fatalf("%s login failed: %d %s", n.name, resp.StatusCode, raw)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "woolwire_session" {
			n.cookie = c
			return
		}
	}
	n.t.Fatalf("%s: no session cookie returned", n.name)
}

type nodeState struct {
	DisplayName string `json:"display_name"`
	MemberID    string `json:"member_id"`
	Room        *struct {
		RoomID         string `json:"room_id"`
		RoomName       string `json:"room_name"`
		Role           string `json:"role"`
		InvitationCode string `json:"invitation_code"`
	} `json:"room"`
}

func (n *node) state() nodeState {
	n.t.Helper()
	var s nodeState
	n.mustDo("GET", "/api/v1/state", nil, &s)
	return s
}

func (n *node) transportAddress() string {
	n.t.Helper()
	var metrics struct {
		Connectivity struct {
			TailcatAddr string `json:"tailcat_addr"`
		} `json:"connectivity"`
	}
	n.mustDo("GET", "/api/v1/metrics", nil, &metrics)
	return metrics.Connectivity.TailcatAddr
}

// TestTwoNodesJoinRemoveAndRestart is the end-to-end gate over real Tailcat.
func TestTwoNodesJoinRemoveAndRestart(t *testing.T) {
	binary := buildBinary(t)

	creator := newNode(t, binary, "creator")
	creator.start()
	creator.login()

	member := newNode(t, binary, "member")
	member.start()
	member.login()

	// The creator must have a usable transport address before it can invite.
	creatorAddr := creator.transportAddress()
	if creatorAddr == "" {
		t.Fatal("creator reported no transport address")
	}
	if strings.HasPrefix(creatorAddr, "privkey:") {
		t.Fatalf("the creator's node private key leaked through metrics: %q", creatorAddr)
	}

	creator.mustDo("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"}, nil)
	member.mustDo("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"}, nil)

	var hosted struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	creator.mustDo("POST", "/api/v1/room/host", map[string]string{"room_name": "Integration Room"}, &hosted)

	var joined struct {
		Status string `json:"status"`
		RoomID string `json:"room_id"`
	}
	member.mustDo("POST", "/api/v1/room/join",
		map[string]string{"invitation_code": hosted.InvitationCode}, &joined)
	if joined.Status != "admitted" {
		t.Fatalf("member was not admitted: %+v", joined)
	}

	memberState := member.state()
	if memberState.Room == nil || memberState.Room.RoomName != "Integration Room" {
		t.Fatalf("member did not learn the room name: %+v", memberState.Room)
	}
	if memberState.Room.InvitationCode != "" {
		t.Fatal("a member must not hold the invitation code")
	}

	t.Run("restart preserves the transport address and the room", func(t *testing.T) {
		addrBefore := member.transportAddress()
		roomBefore := member.state()

		// The session cookie is derived from persisted state, so it keeps
		// working across a restart without logging in again.
		member.restart()

		addrAfter := member.transportAddress()
		if addrAfter != addrBefore {
			t.Fatalf("transport address changed across restart: %q -> %q", addrBefore, addrAfter)
		}

		roomAfter := member.state()
		if roomAfter.Room == nil || roomAfter.Room.RoomID != roomBefore.Room.RoomID {
			t.Fatalf("room did not survive restart: %+v", roomAfter.Room)
		}
		if roomAfter.DisplayName != "Bob" {
			t.Fatalf("identity did not survive restart: %+v", roomAfter)
		}
	})

	t.Run("removal propagates between processes", func(t *testing.T) {
		memberID := member.state().MemberID
		if memberID == "" {
			t.Fatal("member has no member id")
		}

		creator.mustDo("POST", "/api/v1/room-admin/members/"+memberID+"/remove", map[string]any{}, nil)

		var members []struct {
			MemberID string `json:"member_id"`
			Status   string `json:"status"`
		}
		creator.mustDo("GET", "/api/v1/room-admin/members", nil, &members)

		var found bool
		for _, m := range members {
			if m.MemberID == memberID {
				found = true
				if m.Status != "removed" {
					t.Fatalf("member status is %q, want removed", m.Status)
				}
			}
		}
		if !found {
			t.Fatal("the removed member is missing from the creator's roster")
		}

		// The member's own catalog must stop showing the creator's models
		// once it learns it was removed. The poller runs on a 30 second
		// jittered interval, so allow for it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for {
			resp, raw := member.do("GET", "/api/v1/catalog", nil)
			if resp.StatusCode == http.StatusOK {
				var items []map[string]any
				_ = json.Unmarshal(raw, &items)
				if len(items) == 0 {
					return
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("the removed member still sees a catalog after two minutes")
			case <-time.After(5 * time.Second):
			}
		}
	})
}
