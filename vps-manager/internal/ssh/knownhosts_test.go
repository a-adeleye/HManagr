package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return k
}

// TOFU lifecycle: unknown -> trust -> known, plus changed-key detection and
// recovery via Remove.
func TestHostKeyStoreTOFU(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	store := NewHostKeyStore(path)
	host := "example.com:22"
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}
	key1 := testKey(t)

	// First contact, no trust: must report the host as unknown, not accept it.
	err := store.callback(false)(host, addr, key1)
	var unknown *UnknownHostKeyError
	if !errors.As(err, &unknown) {
		t.Fatalf("first contact: want *UnknownHostKeyError, got %v", err)
	}
	if unknown.Fingerprint == "" {
		t.Fatal("UnknownHostKeyError should carry a fingerprint")
	}

	// Trusting it persists the key and accepts the connection.
	if err := store.callback(true)(host, addr, key1); err != nil {
		t.Fatalf("trust new key: %v", err)
	}

	// Now the same key is recognized silently.
	if err := store.callback(false)(host, addr, key1); err != nil {
		t.Fatalf("known key should verify: %v", err)
	}

	// A different key for the same host is a possible MITM — reject it, even when
	// trustNew is set (trust only ever adds first keys, never replaces).
	key2 := testKey(t)
	err = store.callback(true)(host, addr, key2)
	var changed *ChangedHostKeyError
	if !errors.As(err, &changed) {
		t.Fatalf("changed key: want *ChangedHostKeyError, got %v", err)
	}

	// Forgetting the host lets it be trusted again (legitimate rebuild path).
	if err := store.Remove(host); err != nil {
		t.Fatalf("remove: %v", err)
	}
	err = store.callback(false)(host, addr, key2)
	if !errors.As(err, &unknown) {
		t.Fatalf("after remove: want *UnknownHostKeyError, got %v", err)
	}
}

// An empty path disables persistence: every host reads as unknown and trusting
// surfaces a clear error rather than silently succeeding.
func TestHostKeyStoreNoPath(t *testing.T) {
	store := NewHostKeyStore("")
	host := "example.com:22"
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}
	key := testKey(t)

	err := store.callback(false)(host, addr, key)
	var unknown *UnknownHostKeyError
	if !errors.As(err, &unknown) {
		t.Fatalf("want *UnknownHostKeyError, got %v", err)
	}
	if err := store.callback(true)(host, addr, key); err == nil {
		t.Fatal("trusting with no known_hosts path should error, not silently pass")
	}
}
