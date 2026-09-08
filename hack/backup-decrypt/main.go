// Command backup-decrypt turns an encrypted Woolwire export back into a plain
// SQLite database. It shares the decryption path with the export itself, so
// there is only one implementation of the format to keep correct.
//
// It lives under hack/ because it is a recovery aid, not part of the product.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/Chrisbaack/woolwire/internal/localapi"
)

func main() {
	in := flag.String("in", "", "encrypted backup file (.age)")
	out := flag.String("out", "", "destination SQLite database")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: backup-decrypt -in backup.age -out restored.db")
		os.Exit(2)
	}

	passphrase, err := readPassphrase()
	if err != nil {
		fmt.Fprintf(os.Stderr, "read passphrase: %v\n", err)
		os.Exit(1)
	}

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open backup: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	plaintext, err := localapi.DecryptBackup(f, passphrase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decrypt: %v\n", err)
		os.Exit(1)
	}

	// The plaintext is a database full of private keys, so it is never created
	// group- or world-readable.
	if err := os.WriteFile(*out, plaintext, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s (%d bytes)\n", *out, len(plaintext))
}

// readPassphrase prefers the terminal so the passphrase never lands in shell
// history or a process listing. It falls back to stdin for scripted recovery.
func readPassphrase() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Passphrase: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}

	// A whole line, not a whitespace-delimited token: passphrases contain
	// spaces, and Fscanln would silently truncate at the first one.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
