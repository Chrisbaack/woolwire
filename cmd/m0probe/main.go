// m0probe exercises the first implementation-plan gate without pretending to
// be the Woolwire application. It is intentionally disposable: it only
// proves persistent Tailcat identities, TLS 1.3 above the tunnel, and the
// invitation admission boundary.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cbaack/woolwire/internal/m0"
	"github.com/tailscale/tailcat"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: m0probe room|client")
	}
	switch os.Args[1] {
	case "room":
		room(os.Args[2:])
	case "client":
		client(os.Args[2:])
	default:
		fatal("unknown mode %q", os.Args[1])
	}
}

func room(args []string) {
	flags := flag.NewFlagSet("room", flag.ExitOnError)
	statePath := flags.String("state", "./state/room.json", "protected room state path")
	roomID := flags.String("room-id", "", "room identifier used only on first start")
	invitationPath := flags.String("invitation", "./state/invitation.code", "protected invitation-code path")
	flags.Parse(args)

	state, err := m0.LoadRoom(*statePath, *roomID)
	if err != nil {
		fatal("load room: %v", err)
	}
	endpoint, err := m0.NewRoomEndpoint(*statePath, state, nil, echoHandler)
	if err != nil {
		fatal("create room endpoint: %v", err)
	}
	if err := endpoint.Start(); err != nil {
		fatal("start room endpoint: %v", err)
	}
	defer endpoint.Close()

	state = endpoint.State()
	public := state.AuthorityPrivate.Public().(ed25519.PublicKey)
	invitation, err := loadOrCreateInvitation(*invitationPath, state.RoomID, public, endpoint.Address())
	if err != nil {
		fatal("load invitation: %v", err)
	}
	registry, err := m0.NewInviteRegistry(invitation)
	if err != nil {
		fatal("create invitation registry: %v", err)
	}
	if err := endpoint.SetRegistry(registry); err != nil {
		fatal("set invitation registry: %v", err)
	}

	printJSON(map[string]any{
		"mode":         "room",
		"room_id":      state.RoomID,
		"invitation":   filepath.Clean(*invitationPath),
		"tailcat_addr": endpoint.Address(),
		"protocol":     m0.ProtocolPort,
		"tls_min":      "TLS 1.3",
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func client(args []string) {
	flags := flag.NewFlagSet("client", flag.ExitOnError)
	invitationPath := flags.String("invitation", "./state/invitation.code", "invitation-code path")
	statePath := flags.String("state", "./state/client.json", "protected client state path")
	message := flags.String("message", "m0 probe", "message to echo")
	count := flags.Int("count", 1, "number of concurrent authenticated streams")
	flags.Parse(args)
	if *count < 1 || *count > 32 {
		fatal("count must be between 1 and 32")
	}

	code, err := os.ReadFile(*invitationPath)
	if err != nil {
		fatal("read invitation: %v", err)
	}
	invitation, err := m0.ParseInvitation(strings.TrimSpace(string(code)))
	if err != nil {
		fatal("parse invitation: %v", err)
	}
	device, err := m0.LoadDevice(*statePath)
	if err != nil {
		fatal("load device: %v", err)
	}
	devicePublic, err := m0.DevicePublic(device)
	if err != nil {
		fatal("device identity: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, *count)
	for n := 0; n < *count; n++ {
		wg.Add(1)
		go func(sequence int) {
			defer wg.Done()
			client, err := m0.NewClient(invitation, device)
			if err != nil {
				results <- err
				return
			}
			defer client.Close()
			conn, err := m0.DialTLS(ctx, client, invitation, device)
			if err != nil {
				results <- err
				return
			}
			stopContext, err := m0.BindConnContext(ctx, conn)
			if err != nil {
				_ = conn.Close()
				results <- err
				return
			}
			defer stopContext()
			defer conn.Close()
			if err := m0.WriteFrame(conn, m0.Hello{
				Version:         1,
				RoomID:          invitation.RoomID,
				InvitationID:    invitation.InvitationID,
				AdmissionSecret: invitation.AdmissionSecret,
				DevicePublic:    devicePublic,
			}); err != nil {
				results <- err
				return
			}
			var ack m0.HelloAck
			if err := m0.ReadFrame(conn, &ack); err != nil {
				results <- err
				return
			}
			if !ack.Accepted {
				results <- fmt.Errorf("admission rejected: %s", ack.Reason)
				return
			}
			if err := m0.WriteFrame(conn, m0.ProbeMessage{Type: "echo", Body: fmt.Sprintf("%d:%s", sequence, *message)}); err != nil {
				results <- err
				return
			}
			var reply m0.ProbeReply
			if err := m0.ReadFrame(conn, &reply); err != nil {
				results <- err
				return
			}
			if reply.Type != "echo" {
				results <- fmt.Errorf("unexpected reply type %q", reply.Type)
				return
			}
			results <- nil
		}(n)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			fatal("probe failed: %v", err)
		}
	}
	printJSON(map[string]any{"mode": "client", "streams": *count, "room_id": invitation.RoomID, "result": "ok"})
}

func echoHandler(conn *tls.Conn, _ m0.Hello) {
	for {
		var message m0.ProbeMessage
		if err := m0.ReadFrame(conn, &message); err != nil {
			return
		}
		if err := m0.WriteFrame(conn, m0.ProbeReply{Type: message.Type, Body: message.Body}); err != nil {
			return
		}
	}
}

func loadOrCreateInvitation(path, roomID string, authority ed25519.PublicKey, address tailcat.Addr) (m0.Invitation, error) {
	if code, err := os.ReadFile(path); err == nil {
		invitation, parseErr := m0.ParseInvitation(strings.TrimSpace(string(code)))
		if parseErr != nil {
			return m0.Invitation{}, parseErr
		}
		if invitation.RoomID != roomID || invitation.BootstrapAddr != string(address) || invitation.AuthorityPublic != encodePublicKey(authority) {
			return m0.Invitation{}, errors.New("stored invitation does not match current room identity or bootstrap address; rotate it explicitly")
		}
		return invitation, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return m0.Invitation{}, err
	}
	invitation, err := m0.NewInvitation(roomID, authority, address)
	if err != nil {
		return m0.Invitation{}, err
	}
	code, err := invitation.Encode()
	if err != nil {
		return m0.Invitation{}, err
	}
	if err := writeSecretFile(path, []byte(code+"\n")); err != nil {
		return m0.Invitation{}, err
	}
	return invitation, nil
}

func encodePublicKey(public ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(public)
}

func writeSecretFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, contents, 0o600)
}

func printJSON(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fatal("write output: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
