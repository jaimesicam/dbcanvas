// dbcanvas_reset_password resets an admin's password directly in the DBCanvas database.
//
// It ships inside the app image because that is where the problem is: the runtime is
// distroless — no shell, no sqlite3, nothing to fix a forgotten password with — and the
// database is on a volume only that container mounts. Run it against the running app:
//
//	docker exec -it dbcanvas-app-1 /usr/local/bin/dbcanvas_reset_password
//
// It is deliberately its own binary rather than a flag on the server: the server is an
// ENTRYPOINT that starts listening, and an admin recovering a password should not have to
// think about what else invoking it might do.
package main

import (
	"bufio"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dbcanvas_reset_password: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", envOr("DB_PATH", "/data/dbcanvas.db"),
		"path to the DBCanvas database (defaults to $DB_PATH)")
	username := flag.String("user", "",
		"which admin to reset; only needed when there is more than one")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", *dbPath, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("open %s: %w", *dbPath, err)
	}
	// sqlite creates the file on open, so a wrong -db path otherwise fails later with a raw
	// "no such table: users". Say which path was opened and what was expected there.
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'").Scan(&tables); err != nil {
		return fmt.Errorf("read %s: %w", *dbPath, err)
	}
	if tables == 0 {
		return fmt.Errorf("%s is not a DBCanvas database (no users table) — pass -db, or set DB_PATH", *dbPath)
	}

	id, name, err := pickAdmin(db, *username)
	if err != nil {
		return err
	}

	// The username is printed before the prompt, not only after the change: on a stack whose
	// admin was named something other than "admin", knowing which account you are about to
	// change is the point of running this.
	fmt.Printf("Resetting the password for admin user %q.\n", name)
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash the password: %w", err)
	}
	if _, err := db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", string(hash), id); err != nil {
		return fmt.Errorf("update the password: %w", err)
	}
	// Every existing session for this account is dropped. A password reset that leaves a
	// stolen cookie working is not a password reset.
	res, err := db.Exec("DELETE FROM sessions WHERE user_id = ?", id)
	if err != nil {
		return fmt.Errorf("sign out existing sessions: %w", err)
	}
	n, _ := res.RowsAffected()

	fmt.Printf("\nPassword changed for admin user %q.\n", name)
	switch n {
	case 0:
		fmt.Println("No active sessions to sign out.")
	case 1:
		fmt.Println("1 active session was signed out.")
	default:
		fmt.Printf("%d active sessions were signed out.\n", n)
	}
	fmt.Printf("Sign in as %q with the new password.\n", name)
	return nil
}

// pickAdmin resolves which account to reset: the named one, or the only admin there is.
func pickAdmin(db *sql.DB, want string) (int64, string, error) {
	want = strings.TrimSpace(want)
	if want != "" {
		var id int64
		var role string
		err := db.QueryRow("SELECT id, role FROM users WHERE username = ?", want).Scan(&id, &role)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", fmt.Errorf("no user named %q", want)
		}
		if err != nil {
			return 0, "", err
		}
		if role != "admin" {
			return 0, "", fmt.Errorf("%q is not an admin (role %q) — this tool resets admin passwords", want, role)
		}
		return id, want, nil
	}

	rows, err := db.Query("SELECT id, username FROM users WHERE role = 'admin' ORDER BY id")
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	type admin struct {
		id   int64
		name string
	}
	var admins []admin
	for rows.Next() {
		var a admin
		if err := rows.Scan(&a.id, &a.name); err != nil {
			return 0, "", err
		}
		admins = append(admins, a)
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	switch len(admins) {
	case 0:
		return 0, "", errors.New("this database has no admin user")
	case 1:
		return admins[0].id, admins[0].name, nil
	}
	names := make([]string, 0, len(admins))
	for _, a := range admins {
		names = append(names, a.name)
	}
	return 0, "", fmt.Errorf("this database has %d admins (%s) — name one with -user",
		len(admins), strings.Join(names, ", "))
}

// readNewPassword prompts twice and returns the password only if both match and it satisfies
// the same rule the sign-up form does.
func readNewPassword() (string, error) {
	first, err := prompt("New password: ")
	if err != nil {
		return "", err
	}
	// Checked before the confirmation so a too-short password is not typed twice.
	if len(first) < minPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	again, err := prompt("Confirm new password: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passwords do not match — nothing was changed")
	}
	return first, nil
}

// minPasswordLen matches credentials.validate in the server: a password this tool sets must
// be one the sign-in form would have accepted.
const minPasswordLen = 8

// prompt reads one line with the terminal echo off.
//
// The echo is turned off through termios directly rather than with golang.org/x/term:
// requiring that module pulled the whole graph backwards — `go get golang.org/x/term`
// downgraded golang.org/x/crypto from v0.53.0 to v0.26.0 to satisfy it, which is not a
// trade worth making for one ioctl. golang.org/x/sys is already in the build.
//
// Without a terminal — a password piped in by a script — it falls back to a plain read and
// says so, rather than failing.
func prompt(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	restore, echoOff := disableEcho(fd)
	if !echoOff {
		fmt.Print(label + "(input is not a terminal, so it will be echoed) ")
		return readLine()
	}
	defer restore()
	fmt.Print(label)
	line, err := readLine()
	fmt.Println() // the user's Enter was not echoed either
	return line, err
}

// disableEcho turns terminal echo off, returning a restore function. The second return is
// false when stdin is not a terminal at all, which is not an error — see prompt.
func disableEcho(fd int) (func(), bool) {
	before, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}, false
	}
	after := *before
	after.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &after); err != nil {
		return func() {}, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, before) }, true
}

// stdin is buffered once for the whole process, not per prompt: a bufio.Reader reads ahead,
// so a second reader over the same file would find the confirmation line already swallowed by
// the first and fail with EOF. Piping both lines in is exactly how that shows up.
var stdin = bufio.NewReader(os.Stdin)

// readLine reads to the newline. A password may hold spaces, which is why this is not
// fmt.Scanln — that stops at the first one and would silently truncate.
func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read the password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
