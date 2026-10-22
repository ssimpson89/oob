// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// Adapted from bauddy (internal/share/lock_unix.go): the lock that elects the
// owner, and the private folder for sockets, keyed by oob's state directory.

//go:build unix

package share

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

// dir returns the folder for sockets and locks, made if needed and checked to
// be this user's alone: ${XDG_STATE_HOME:-~/.local/state}/oob/share.
func dir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	d := filepath.Join(base, "oob", "share")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	var st unix.Stat_t
	if err := unix.Lstat(d, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s isn't a folder you own, so this console can't be shared", d)
	}
	if st.Mode&0o077 != 0 {
		if err := os.Chmod(d, 0o700); err != nil {
			return "", fmt.Errorf("%s is open to other users, so this console can't be shared", d)
		}
	}
	return d, nil
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// paths returns the socket and lock for a host at addr, or ok false if it can't
// be shared here. The host name alone doesn't say which console it is: the
// config can point the same name at another BMC between two oob processes, and
// they must not end up on one console. So a name with an address is followed
// by a hash of both. A host name that isn't safe in a file name is hashed.
func paths(host, addr string) (sock, lockPath string, ok bool) {
	d, err := dir()
	if err != nil {
		return "", "", false
	}
	name := host
	if !safeName.MatchString(host) {
		sum := sha256.Sum256([]byte(host))
		name = "h-" + hex.EncodeToString(sum[:8])
	}
	if addr != "" {
		if len(name) > 24 {
			name = name[:24]
		}
		sum := sha256.Sum256([]byte(host + "\x00" + addr))
		name += "-" + hex.EncodeToString(sum[:6])
	}
	sock = filepath.Join(d, name+".sock")
	if len(sock) > 100 { // the socket path limit is about 104 bytes
		return "", "", false
	}
	return sock, filepath.Join(d, name+".lock"), true
}

// checkSocket refuses a socket that isn't ours: a joiner would hand its
// console to whoever made it.
func checkSocket(path string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("%s isn't a socket", path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user", path)
	}
	return nil
}

var errHeld = errors.New("another oob holds this console")

// lock takes the lock file at path, or returns errHeld if another process has
// it. The OS releases it when the process ends, so it can't go stale. The
// holder removes the file when it lets go (unlock), so if the file locked here
// has gone from path meanwhile, try again with a fresh one.
func lock(path string) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, err
		}
		lockOpened()
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, errHeld
			}
			return nil, err
		}
		locked, err1 := f.Stat()
		now, err2 := os.Lstat(path)
		if err1 == nil && err2 == nil && os.SameFile(locked, now) {
			return f, nil
		}
		f.Close()
	}
}

// lockOpened runs between opening a lock file and locking it; tests pause there.
var lockOpened = func() {}

// unlock lets go of a lock and removes its file. Removed while still held, so
// whoever opens the path next gets a new file (see lock).
func unlock(f *os.File) {
	_ = os.Remove(f.Name())
	f.Close()
}
