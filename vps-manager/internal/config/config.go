package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// VPS is a single saved server configuration.
//
// NOTE: Password is stored plaintext in the config file. This is fine for a
// local-only prototype but you should swap it for an OS-keychain integration
// (e.g. github.com/zalando/go-keyring) before trusting it with real secrets.
type VPS struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	AuthType string `json:"authType"` // "key" or "password"
	KeyPath  string `json:"keyPath,omitempty"`
	Password string `json:"password,omitempty"`
}

type fileData struct {
	VPSes []VPS `json:"vpses"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	data fileData
}

func New() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("locate config dir: %w", err)
	}
	appDir := filepath.Join(dir, "vps-manager")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	s := &Store{path: filepath.Join(appDir, "config.json")}
	s.load()
	return s, nil
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return // empty store is fine
	}
	_ = json.Unmarshal(b, &s.data)
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) List() []VPS {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]VPS, len(s.data.VPSes))
	copy(out, s.data.VPSes)
	return out
}

func (s *Store) Get(id string) (VPS, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.data.VPSes {
		if v.ID == id {
			return v, true
		}
	}
	return VPS{}, false
}

func (s *Store) Add(v VPS) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = newID()
	}
	if v.Port == 0 {
		v.Port = 22
	}
	s.data.VPSes = append(s.data.VPSes, v)
	return s.save()
}

func (s *Store) Update(v VPS) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.data.VPSes {
		if existing.ID == v.ID {
			if v.Port == 0 {
				v.Port = 22
			}
			s.data.VPSes[i] = v
			return s.save()
		}
	}
	return fmt.Errorf("vps %s not found", v.ID)
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.data.VPSes {
		if v.ID == id {
			s.data.VPSes = append(s.data.VPSes[:i], s.data.VPSes[i+1:]...)
			return s.save()
		}
	}
	return nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "vps_" + hex.EncodeToString(b[:])
}
