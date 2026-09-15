package cli

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"runtime"

	"github.com/pterm/pterm"
	"golang.org/x/term"
)

const (
	// minPasswordLen is the minimum accepted admin password length (bytes).
	minPasswordLen = 8
	// maxPasswordLen is bcrypt's hard input limit; longer inputs would be
	// silently truncated by older implementations and are rejected by ours.
	maxPasswordLen = 72
	// generatedPasswordLen is the length of --generate passwords.
	generatedPasswordLen = 20
)

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatePassword returns a random password drawn from an unambiguous
// alphanumeric alphabet (no 0/O, 1/l/I) so it can be copied by hand.
func generatePassword() (string, error) {
	out := make([]byte, generatedPasswordLen)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// validatePassword enforces the length policy shared by every entry path.
func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	if len(pw) > maxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes (bcrypt limit)", maxPasswordLen)
	}
	return nil
}

// warnArgvSecret reminds the operator that a secret passed as a flag is
// visible to other local users and lands in the shell history.
func warnArgvSecret(flag string) {
	pterm.Warning.Printfln("%s was passed on the command line; it is visible in `ps` and your shell history. Prefer the interactive prompt or --generate.", flag)
}

// promptPassword reads a password twice from the controlling terminal without
// echo and returns it once both entries match and pass validation.
func promptPassword(prompt string) (string, error) {
	tty, fd, closer, err := openTTY()
	if err != nil {
		return "", err
	}
	defer closer()

	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(tty, "%s: ", prompt)
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(tty)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if err := validatePassword(string(first)); err != nil {
			fmt.Fprintf(tty, "  %v, try again\n", err)
			continue
		}
		fmt.Fprintf(tty, "%s (again): ", prompt)
		second, err := term.ReadPassword(fd)
		fmt.Fprintln(tty)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if !bytes.Equal(first, second) {
			fmt.Fprintln(tty, "  passwords do not match, try again")
			continue
		}
		return string(first), nil
	}
	return "", errors.New("too many failed attempts")
}

// openTTY returns a handle to the controlling terminal for prompting. It
// falls back to stdin when /dev/tty is unavailable (Windows, some
// containers) and fails when stdin is not a terminal so scripts cannot hang.
func openTTY() (out *os.File, fd int, closer func(), err error) {
	if runtime.GOOS != "windows" {
		if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
			if term.IsTerminal(int(f.Fd())) {
				return f, int(f.Fd()), func() { f.Close() }, nil
			}
			f.Close()
		}
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stderr, int(os.Stdin.Fd()), func() {}, nil
	}
	return nil, 0, nil, errors.New("no terminal available to prompt for a password; use --generate or --password=<pw>")
}

// resolvePassword picks the password source for admin create/password:
// an explicit flag value, a generated one, or an interactive prompt.
// generated reports whether the returned password must be shown to the user.
func resolvePassword(customPassword string, generate bool) (password string, generated bool, err error) {
	switch {
	case customPassword != "" && generate:
		return "", false, errors.New("--password and --generate are mutually exclusive")
	case customPassword != "":
		warnArgvSecret("--password")
		if err := validatePassword(customPassword); err != nil {
			return "", false, err
		}
		return customPassword, false, nil
	case generate:
		pw, err := generatePassword()
		if err != nil {
			return "", false, fmt.Errorf("generate password: %w", err)
		}
		return pw, true, nil
	default:
		pw, err := promptPassword("Password")
		if err != nil {
			return "", false, err
		}
		return pw, false, nil
	}
}
