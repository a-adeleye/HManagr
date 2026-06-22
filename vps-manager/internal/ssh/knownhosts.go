package ssh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyStore verifies and records SSH host keys in an OpenSSH known_hosts
// file, implementing trust-on-first-use (TOFU):
//
//   - A host we've seen before whose key matches is accepted silently.
//   - A host we've never seen is reported back as *UnknownHostKeyError so the
//     caller can show the fingerprint and ask the user to trust it (rather than
//     silently trusting, which would defeat the point).
//   - A host whose presented key does NOT match the stored one is ALWAYS
//     rejected with *ChangedHostKeyError — that's the man-in-the-middle case.
//
// This replaces ssh.InsecureIgnoreHostKey(), which trusted every key blindly.
type HostKeyStore struct {
	path string
	mu   sync.Mutex
}

// NewHostKeyStore returns a store backed by the given known_hosts file. The file
// (and its parent dir) are created lazily on first trust. An empty path disables
// persistence — every host then reads as unknown and trust can't be saved.
func NewHostKeyStore(path string) *HostKeyStore {
	return &HostKeyStore{path: path}
}

// UnknownHostKeyError means the host isn't in known_hosts yet. Trusting it means
// reconnecting with TrustNewHostKey set, which appends the key.
type UnknownHostKeyError struct {
	Host        string
	KeyType     string
	Fingerprint string
}

func (e *UnknownHostKeyError) Error() string {
	return fmt.Sprintf("unknown host key for %s (%s %s)", e.Host, e.KeyType, e.Fingerprint)
}

// ChangedHostKeyError means the presented key differs from the stored one — a
// possible man-in-the-middle. It is never auto-trusted; recovering requires an
// explicit Remove (e.g. after a legitimate server rebuild).
type ChangedHostKeyError struct {
	Host        string
	KeyType     string
	Fingerprint string
}

func (e *ChangedHostKeyError) Error() string {
	return fmt.Sprintf("remote host key changed for %s (now %s %s) — possible man-in-the-middle", e.Host, e.KeyType, e.Fingerprint)
}

// callback returns an ssh.HostKeyCallback enforcing the policy above. trustNew
// controls the never-seen-before case: false reports *UnknownHostKeyError; true
// appends the key and accepts it (used after the user agreed to trust it).
func (s *HostKeyStore) callback(trustNew bool) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		s.mu.Lock()
		defer s.mu.Unlock()

		verify, err := s.loadCallback()
		if err != nil {
			return err
		}
		if verify != nil {
			vErr := verify(hostname, remote, key)
			if vErr == nil {
				return nil // known host, key matches
			}
			var ke *knownhosts.KeyError
			if errors.As(vErr, &ke) {
				if len(ke.Want) > 0 {
					// Host is present but the key doesn't match any stored entry.
					return &ChangedHostKeyError{
						Host:        hostname,
						KeyType:     key.Type(),
						Fingerprint: ssh.FingerprintSHA256(key),
					}
				}
				// len(Want) == 0: host not found at all — fall through to TOFU.
			} else {
				// e.g. a *knownhosts.RevokedError or an I/O error: don't trust.
				return vErr
			}
		}

		if !trustNew {
			return &UnknownHostKeyError{
				Host:        hostname,
				KeyType:     key.Type(),
				Fingerprint: ssh.FingerprintSHA256(key),
			}
		}
		return s.appendKey(hostname, key)
	}
}

// loadCallback builds a verifying callback from the known_hosts file, or nil if
// the file doesn't exist yet (nothing trusted → every host is unknown).
func (s *HostKeyStore) loadCallback() (ssh.HostKeyCallback, error) {
	if s.path == "" {
		return nil, nil
	}
	if _, err := os.Stat(s.path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return knownhosts.New(s.path)
}

// appendKey writes a plain known_hosts line for hostname=>key, creating the file
// (0600) and its parent dir if needed.
func (s *HostKeyStore) appendKey(hostname string, key ssh.PublicKey) error {
	if s.path == "" {
		return fmt.Errorf("no known_hosts path configured; cannot persist host key")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// Remove drops every stored entry for the given host (in "host:port" form),
// letting the next connection re-trust it. Used to recover from a legitimate
// server rebuild that tripped ChangedHostKeyError. Only the plain (unhashed)
// lines this store writes are matched; hashed OpenSSH entries are left alone.
func (s *HostKeyStore) Remove(hostname string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	target := knownhosts.Normalize(hostname)
	lines := strings.Split(string(b), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		fields := strings.Fields(trimmed)
		matched := false
		for _, h := range strings.Split(fields[0], ",") {
			if h == target {
				matched = true
				break
			}
		}
		if !matched {
			kept = append(kept, line)
		}
	}
	out := strings.Join(kept, "\n")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
