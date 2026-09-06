package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"syscall"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/localapi"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
	"github.com/cbaack/woolwire/web"
	"tailscale.com/types/key"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listenAddr := flag.String("listen", getEnv("WOOLWIRE_LISTEN", "127.0.0.1:7070"), "local HTTP listen address")
	stateDir := flag.String("state", getEnv("WOOLWIRE_STATE_DIR", "./state"), "persistent state directory")
	peerPort := flag.Int("peer-port", 4242, "peer transport protocol port")
	modelsDir := flag.String("models", getEnv("WOOLWIRE_MODELS_DIR", "./models"), "directory for model weights")
	runnerURL := flag.String("runner-url", getEnv("WOOLWIRE_RUNNER_URL", ""), "runner controller URL")
	runnerToken := flag.String("runner-token", getEnv("WOOLWIRE_RUNNER_TOKEN", ""), "runner authorization token")
	allowedHostsStr := flag.String("allowed-hosts", getEnv("WOOLWIRE_ALLOWED_HOSTS", ""), "comma-separated extra allowed hostnames for local API")
	setupTokenFlag := flag.String("setup-token", getEnv("WOOLWIRE_SETUP_TOKEN", ""), "custom setup secret or PIN (optional)")
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

	// Load or create device identity
	ident, err := dbStore.GetDeviceIdentity()
	if err != nil {
		dev, genErr := identity.GenerateDevice()
		if genErr != nil {
			fmt.Fprintf(os.Stderr, "failed to generate device identity: %v\n", genErr)
			os.Exit(1)
		}
		nodeKey := key.NewNode()
		keyBytes, _ := nodeKey.MarshalText()
		ident = &store.DeviceIdentity{
			TailcatKey:    string(keyBytes),
			DevicePrivate: dev.PrivateKey,
			DeviceCertDER: dev.CertDER,
			DevicePublic:  dev.EncodedPublicKey(),
		}
		if saveErr := dbStore.SaveDeviceIdentity(*ident); saveErr != nil {
			fmt.Fprintf(os.Stderr, "failed to save device identity: %v\n", saveErr)
			os.Exit(1)
		}
	}

	// Tailcat transport
	var nodePrivate key.NodePrivate
	_ = nodePrivate.UnmarshalText([]byte(ident.TailcatKey))
	trans, err := transport.NewTailcatTransport(transport.TailcatConfig{
		NodeKey: nodePrivate,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize transport: %v\n", err)
		os.Exit(1)
	}
	defer trans.Close()

	// Setup secret
	setupToken := strings.TrimSpace(*setupTokenFlag)
	if setupToken == "" {
		setupToken, _ = dbStore.GetSetting("setup_token")
	}
	if setupToken == "" {
		tokenBytes := make([]byte, 16)
		_, _ = rand.Read(tokenBytes)
		setupToken = hex.EncodeToString(tokenBytes)
	}
	_ = dbStore.SetSetting("setup_token", setupToken)

	// Prepare web static asset filesystem
	distFS, err := fs.Sub(web.Dist, "dist")
	var webAssets http.FileSystem
	if err == nil {
		webAssets = http.FS(distFS)
	}

	var peerServer *peerapi.Server
	startPeer := func(authority ed25519.PrivateKey) error {
		if peerServer != nil {
			_ = peerServer.Close()
		}
		peerServer = peerapi.NewServer(dbStore, authority)
		l, lErr := trans.Listen(uint16(*peerPort))
		if lErr != nil {
			return lErr
		}
		go func() { _ = peerServer.Serve(l) }()
		return nil
	}

	// If already in a room, start peer listener
	if roomRec, rErr := dbStore.GetRoomState(); rErr == nil && roomRec != nil {
		var authPriv ed25519.PrivateKey
		if len(roomRec.AuthorityPrivate) > 0 {
			authPriv = ed25519.PrivateKey(roomRec.AuthorityPrivate)
		}
		_ = startPeer(authPriv)
	}

	var allowedHosts []string
	if *allowedHostsStr != "" {
		for _, h := range strings.Split(*allowedHostsStr, ",") {
			if trimmed := strings.TrimSpace(h); trimmed != "" {
				allowedHosts = append(allowedHosts, trimmed)
			}
		}
	}

	localServer, err := localapi.NewServer(localapi.Config{
		Store:        dbStore,
		Transport:    trans,
		SetupToken:   setupToken,
		PeerPort:     uint16(*peerPort),
		WebAssets:    webAssets,
		StartPeerFn:  startPeer,
		ModelsDir:    *modelsDir,
		RunnerURL:    *runnerURL,
		RunnerToken:  *runnerToken,
		AllowedHosts: allowedHosts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create local server: %v\n", err)
		os.Exit(1)
	}
	defer localServer.Close()

	localListener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", *listenAddr, err)
		os.Exit(1)
	}
	defer localListener.Close()

	fmt.Printf("\n======================================================\n")
	fmt.Printf("Woolwire started successfully!\n")
	fmt.Printf("Web Interface: http://%s\n", *listenAddr)
	fmt.Printf("Transport Address: %s\n", trans.Address())
	fmt.Printf("Setup Token: %s\n", setupToken)
	fmt.Printf("======================================================\n\n")

	go func() {
		_ = localServer.Serve(localListener)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nShutting down Woolwire...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = localServer.Close()
	if peerServer != nil {
		_ = peerServer.Close()
	}
	_ = trans.Close()
	_ = ctx
}
