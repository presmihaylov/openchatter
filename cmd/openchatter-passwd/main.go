// Command openchatter-passwd resets a user's password from the server host:
//
//	openchatter-passwd [-create] <username>
//
// The new password is read from the terminal (hidden) or, when stdin is not a
// terminal, from the first line of stdin; it never appears in argv or ps. It
// writes a fresh bcrypt hash, logs the user out everywhere and flags the
// account so the web UI asks for a new password. -create makes the password
// account when the username is unknown (registration is closed on prod).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/envx"
	"github.com/presmihaylov/openchatter/services/auth"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "openchatter-passwd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("openchatter-passwd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	create := fs.Bool("create", false, "create the password account when the username is unknown")
	usage := errors.New("usage: openchatter-passwd [-create] <username>  (password read from stdin)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return usage
	}
	username := strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	dbURL := envx.Get("DB_URL")
	if dbURL == "" {
		return errors.New("OPENCHATTER_DB_URL is required")
	}
	password, err := readPassword(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := models.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer store.Close()

	n, err := reset(ctx, store, username, password, *create)
	if err != nil {
		return err
	}
	fmt.Printf("password updated for %s; %d session(s) logged out\n", username, n)
	return nil
}

// readPassword prompts without echo on a terminal and otherwise takes one
// line from in, so scripts can pipe the password.
func readPassword(in io.Reader, isTerminal bool) (string, error) {
	var raw []byte
	var err error
	if isTerminal {
		fmt.Fprint(os.Stderr, "New password: ")
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	}
	if !isTerminal {
		raw, err = bufio.NewReader(in).ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	return password, nil
}

// reset stores the new hash and revokes every session in one transaction,
// then flags the account: an operator-set password is a temporary one.
// With create, an unknown username becomes a new account (display name =
// username) instead of an error.
func reset(ctx context.Context, store *models.Store, username, password string, create bool) (int64, error) {
	if create && !auth.ValidUsername(username) {
		return 0, auth.ErrBadUsername
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return 0, err
	}
	userID, _, err := store.PasswordIdentity(ctx, username)
	if errors.Is(err, models.ErrNotFound) && !create {
		return 0, fmt.Errorf("no password account for %q (use -create to add it)", username)
	}
	if errors.Is(err, models.ErrNotFound) {
		u, err := store.CreatePasswordUser(ctx, username, username, hash)
		if err != nil {
			return 0, err
		}
		return 0, store.SetMustChangePassword(ctx, u.ID, true)
	}
	if err != nil {
		return 0, err
	}
	n, err := store.SetPasswordHash(ctx, userID, hash, nil)
	if err != nil {
		return 0, err
	}
	return n, store.SetMustChangePassword(ctx, userID, true)
}
