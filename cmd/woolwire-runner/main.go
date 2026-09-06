package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cbaack/woolwire/internal/runner"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listenAddr := flag.String("listen", getEnv("RUNNER_LISTEN", "0.0.0.0:8080"), "runner controller listen address")
	modelsDir := flag.String("models", getEnv("MODELS_DIR", "/models"), "directory containing model weights")
	runnerToken := flag.String("token", getEnv("RUNNER_TOKEN", ""), "authentication token for runner controller")
	enginePath := flag.String("engine-path", getEnv("ENGINE_PATH", "llama-server"), "path to llama-server binary")
	enginePort := flag.Int("engine-port", 8081, "port for internal llama-server")
	flag.Parse()

	if *modelsDir == "" {
		fmt.Fprintf(os.Stderr, "models directory is required\n")
		os.Exit(1)
	}

	ctrl, err := runner.NewController(runner.Config{
		ModelDir:    *modelsDir,
		RunnerToken: *runnerToken,
		EnginePath:  *enginePath,
		EnginePort:  uint16(*enginePort),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize runner controller: %v\n", err)
		os.Exit(1)
	}

	l, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", *listenAddr, err)
		os.Exit(1)
	}
	defer l.Close()

	fmt.Printf("Woolwire Runner Controller listening on %s (models: %s)\n", *listenAddr, *modelsDir)

	go func() {
		if err := ctrl.Serve(l); err != nil {
			fmt.Fprintf(os.Stderr, "runner controller error: %v\n", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("Shutting down runner controller...")
}
