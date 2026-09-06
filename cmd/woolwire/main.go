package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/localapi"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
	"github.com/cbaack/woolwire/web"
	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// peerManager owns the peer server and its listeners. The peer server is
// created and replaced from HTTP handler goroutines (hosting or joining a
// room), so every access to it is behind this mutex.
type peerManager struct {
	mu        sync.Mutex
	server    *peerapi.Server
	listeners []net.Listener

	store    *store.Store
	trans    transport.Transport
	peerPort uint16
	localAPI func() *localapi.Server
}

func (p *peerManager) start(authority ed25519.PrivateKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startLocked(authority)
}

func (p *peerManager) startLocked(authority ed25519.PrivateKey) error {
	p.stopLocked()

	dev, err := p.store.GetDeviceIdentity()
	if err != nil || dev == nil {
		return fmt.Errorf("device identity missing: %w", err)
	}
	deviceCert := tls.Certificate{
		Certificate: [][]byte{dev.DeviceCertDER},
		PrivateKey:  ed25519.PrivateKey(dev.DevicePrivate),
	}

	// The creator serves bootstrap under a certificate signed by the room
	// authority, which is the only thing a joiner can verify from the
	// invitation code alone.
	var roomCert *tls.Certificate
	if roomRec, err := p.store.GetRoomState(); err == nil && roomRec != nil && len(roomRec.RoomCertDER) > 0 {
		roomCert = &tls.Certificate{
			Certificate: [][]byte{roomRec.RoomCertDER},
			PrivateKey:  ed25519.PrivateKey(dev.DevicePrivate),
		}
	}

	cfg := peerapi.Config{
		Store:      p.store,
		Authority:  authority,
		DeviceCert: deviceCert,
		RoomCert:   roomCert,
	}
	if local := p.localAPI(); local != nil {
		cfg.Inference = local.Inference()
	}

	srv := peerapi.NewServer(cfg)

	peerListener, err := p.trans.Listen(p.peerPort)
	if err != nil {
		return err
	}
	p.listeners = append(p.listeners, peerListener)
	go func() { _ = srv.Serve(tls.NewListener(peerListener, srv.PeerTLSConfig())) }()

	// Bootstrap sits on its own port because it must accept a device
	// certificate the roster has never seen, which the peer listener must not.
	if bootstrapCfg := srv.BootstrapTLSConfig(); bootstrapCfg != nil {
		bootstrapListener, err := p.trans.Listen(p.peerPort + peerapi.BootstrapPortOffset)
		if err != nil {
			p.stopLocked()
			return err
		}
		p.listeners = append(p.listeners, bootstrapListener)
		go func() { _ = srv.ServeBootstrap(tls.NewListener(bootstrapListener, bootstrapCfg)) }()
	}

	p.server = srv
	return nil
}

func (p *peerManager) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	return nil
}

func (p *peerManager) stopLocked() {
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = p.server.Shutdown(ctx)
		cancel()
		p.server = nil
	}
	for _, l := range p.listeners {
		_ = l.Close()
	}
	p.listeners = nil
}

func (p *peerManager) shutdown(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		_ = p.server.Shutdown(ctx)
		p.server = nil
	}
	for _, l := range p.listeners {
		_ = l.Close()
	}
	p.listeners = nil
}

func main() {
	listenAddr := flag.String("listen", getEnv("WOOLWIRE_LISTEN", "127.0.0.1:7070"), "local HTTP listen address")
	stateDir := flag.String("state", getEnv("WOOLWIRE_STATE_DIR", "./state"), "persistent state directory")
	peerPort := flag.Int("peer-port", 4242, "peer transport protocol port")
	modelsDir := flag.String("models", getEnv("WOOLWIRE_MODELS_DIR", "./models"), "directory for model weights")
	runnerURL := flag.String("runner-url", getEnv("WOOLWIRE_RUNNER_URL", ""), "runner controller URL")
	runnerToken := flag.String("runner-token", getEnv("WOOLWIRE_RUNNER_TOKEN", ""), "runner authorization token")
	allowedHostsStr := flag.String("allowed-hosts", getEnv("WOOLWIRE_ALLOWED_HOSTS", ""), "comma-separated extra allowed hostnames for local API")
	setupTokenFlag := flag.String("setup-token", getEnv("WOOLWIRE_SETUP_TOKEN", ""), "custom setup secret (optional, minimum 12 characters)")
	flag.Parse()

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create state dir: %v\n", err)
		os.Exit(1)
	}

	dbPath := filepath.Join(*stateDir, "woolwire.db")
	dbStore, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer dbStore.Close()

	ident, err := loadOrCreateIdentity(dbStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	trans, err := startTransport(dbStore, ident)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize transport: %v\n", err)
		os.Exit(1)
	}
	defer trans.Close()

	setupToken, freshSecret, err := resolveSetupSecret(dbStore, strings.TrimSpace(*setupTokenFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	distFS, err := fs.Sub(web.Dist, "dist")
	var webAssets http.FileSystem
	if err == nil {
		webAssets = http.FS(distFS)
	}

	var localServer *localapi.Server
	peers := &peerManager{
		store:    dbStore,
		trans:    trans,
		peerPort: uint16(*peerPort),
		localAPI: func() *localapi.Server { return localServer },
	}

	var allowedHosts []string
	for _, h := range strings.Split(*allowedHostsStr, ",") {
		if trimmed := strings.TrimSpace(h); trimmed != "" {
			allowedHosts = append(allowedHosts, trimmed)
		}
	}

	localServer, err = localapi.NewServer(localapi.Config{
		Store:        dbStore,
		Transport:    trans,
		SetupToken:   setupToken,
		PeerPort:     uint16(*peerPort),
		WebAssets:    webAssets,
		StartPeerFn:  peers.start,
		StopPeerFn:   peers.stop,
		ModelsDir:    *modelsDir,
		StateDir:     *stateDir,
		RunnerURL:    *runnerURL,
		RunnerToken:  *runnerToken,
		AllowedHosts: allowedHosts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create local server: %v\n", err)
		os.Exit(1)
	}

	// Resume serving peers if this node is already in a room.
	if roomRec, rErr := dbStore.GetRoomState(); rErr == nil && roomRec != nil {
		var authPriv ed25519.PrivateKey
		if len(roomRec.AuthorityPrivate) > 0 {
			authPriv = ed25519.PrivateKey(roomRec.AuthorityPrivate)
		}
		if err := peers.start(authPriv); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to start peer listener: %v\n", err)
		}
	}

	localListener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", *listenAddr, err)
		os.Exit(1)
	}

	fmt.Printf("\n======================================================\n")
	fmt.Printf("Woolwire started successfully!\n")
	fmt.Printf("Web Interface: http://%s\n", *listenAddr)
	fmt.Printf("Transport Address: %s\n", trans.Address())
	if freshSecret != "" {
		// Printed once, on the boot that generated it. It is single-use and is
		// invalidated the moment it is exchanged for a session.
		fmt.Printf("Setup Secret (one-time, shown only now): %s\n", freshSecret)
	} else {
		fmt.Printf("Setup Secret: already issued; reset it from Settings if you need a new one.\n")
	}
	fmt.Printf("======================================================\n\n")

	localServer.StartBackground()
	go func() {
		_ = localServer.Serve(localListener)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down Woolwire...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown drains in-flight requests; Close cut them off mid-response.
	_ = localServer.Shutdown(ctx)
	peers.shutdown(ctx)
	_ = trans.Close()
	_ = localListener.Close()
}

func loadOrCreateIdentity(dbStore *store.Store) (*store.DeviceIdentity, error) {
	ident, err := dbStore.GetDeviceIdentity()
	if err == nil && ident != nil {
		return ident, nil
	}

	dev, genErr := identity.GenerateDevice()
	if genErr != nil {
		return nil, fmt.Errorf("failed to generate device identity: %w", genErr)
	}
	nodeKey := key.NewNode()
	keyBytes, _ := nodeKey.MarshalText()
	psk := tailcat.NewPresharedKey()
	pskBytes, err := psk.MarshalText()
	if err != nil {
		return nil, fmt.Errorf("failed to encode transport preshared key: %w", err)
	}

	ident = &store.DeviceIdentity{
		TailcatKey:    string(keyBytes),
		DevicePrivate: dev.PrivateKey,
		DeviceCertDER: dev.CertDER,
		DevicePublic:  dev.EncodedPublicKey(),
		TailcatPSK:    string(pskBytes),
	}
	if saveErr := dbStore.SaveDeviceIdentity(*ident); saveErr != nil {
		return nil, fmt.Errorf("failed to save device identity: %w", saveErr)
	}
	return ident, nil
}

// startTransport restores the full Tailcat identity. The pre-shared key and
// the resolved DERP region are both embedded in the address peers stored and
// the invitation code carries, so restoring only the node key produced a new
// address on every restart and silently invalidated both.
func startTransport(dbStore *store.Store, ident *store.DeviceIdentity) (*transport.TailcatTransport, error) {
	var nodePrivate key.NodePrivate
	if err := nodePrivate.UnmarshalText([]byte(ident.TailcatKey)); err != nil {
		return nil, fmt.Errorf("restore transport node key: %w", err)
	}

	var psk tailcat.PresharedKey
	if ident.TailcatPSK != "" {
		if err := psk.UnmarshalText([]byte(ident.TailcatPSK)); err != nil {
			return nil, fmt.Errorf("restore transport preshared key: %w", err)
		}
	}

	trans, err := transport.NewTailcatTransport(transport.TailcatConfig{
		NodeKey:      nodePrivate,
		PresharedKey: psk,
		Address:      ident.TailcatAddr,
	})
	if err != nil {
		return nil, err
	}

	// Persist whatever was newly generated so the next boot reproduces this
	// exact address.
	changed := false
	if ident.TailcatPSK == "" {
		if b, err := trans.PresharedKey().MarshalText(); err == nil {
			ident.TailcatPSK = string(b)
			changed = true
		}
	}
	if ident.TailcatAddr != trans.Address() {
		ident.TailcatAddr = trans.Address()
		changed = true
	}
	if changed {
		_ = dbStore.SaveDeviceIdentity(*ident)
	}

	return trans, nil
}

// resolveSetupSecret returns the active setup secret and, when one was minted
// on this boot, the plaintext to print exactly once.
func resolveSetupSecret(dbStore *store.Store, override string) (secret string, fresh string, err error) {
	if override != "" {
		if len(override) < 12 {
			return "", "", fmt.Errorf("setup secret must be at least 12 characters")
		}
		_ = dbStore.SetSetting("setup_token", override)
		_ = dbStore.SetSetting("setup_token_used", "false")
		return override, "", nil
	}

	stored, _ := dbStore.GetSetting("setup_token")
	used, _ := dbStore.GetSetting("setup_token_used")
	if stored != "" && used != "true" {
		// Already issued on an earlier boot and not yet redeemed. It is not
		// reprinted: every reprint is another copy that can leak from a log.
		return stored, "", nil
	}
	if used == "true" {
		return "", "", nil
	}

	generated, genErr := generateSetupSecret()
	if genErr != nil {
		return "", "", fmt.Errorf("failed to generate setup secret: %w", genErr)
	}
	_ = dbStore.SetSetting("setup_token", generated)
	_ = dbStore.SetSetting("setup_token_used", "false")
	return generated, generated, nil
}

// generateSetupSecret mints a 128-bit one-time pairing secret.
func generateSetupSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
