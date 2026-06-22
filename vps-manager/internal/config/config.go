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
	// IsLocal marks the virtual "this machine" entry. It is never persisted —
	// the App layer synthesizes it — but the field rides along so the frontend
	// can branch its UI on it.
	IsLocal bool `json:"isLocal,omitempty"`
}

// EnvVar is one environment variable for a deployment. Secret only affects how
// the UI renders it (masked); storage is the same plaintext JSON as everything
// else in this file — see the keychain caveat above.
type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

// Deployment is a GitHub-repo-to-VPS deployment definition (Coolify-style):
// clone/pull the repo on the VPS, write a .env from EnvVars, and bring the
// stack up with docker compose.
type Deployment struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	VPSID       string   `json:"vpsId"`
	RepoURL     string   `json:"repoUrl"`
	Branch      string   `json:"branch"`
	Path        string   `json:"path"`                  // checkout dir on the VPS
	ComposeFile string   `json:"composeFile,omitempty"` // auto-detected when empty
	GithubToken string   `json:"githubToken,omitempty"` // for private repos over https
	EnvVars     []EnvVar `json:"envVars"`
	UseSudo     bool     `json:"useSudo"`
	LastDeploy  string   `json:"lastDeploy,omitempty"` // RFC3339
	LastStatus  string   `json:"lastStatus,omitempty"` // running | success | failed
	LastCommit  string   `json:"lastCommit,omitempty"`
}

// Project is a named server+path bookmark: selecting it connects to VPSID (or
// reuses a live connection) and roots the session — file browser, terminal — at
// Path. It's a convenience layer over an existing server, not a new connection.
type Project struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	VPSID string `json:"vpsId"`
	Path  string `json:"path"`
	// Database optionally pins which database the DB tab shows for this project.
	// Empty falls back to the container's configured default (POSTGRES_DB / etc).
	Database string `json:"database,omitempty"`
}

type fileData struct {
	VPSes       []VPS        `json:"vpses"`
	Deployments []Deployment `json:"deployments,omitempty"`
	Projects    []Project    `json:"projects,omitempty"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	data fileData
}

// KnownHostsPath is the app's SSH known_hosts file, kept next to config.json in
// the user config dir. Best-effort: returns "" if the config dir can't be
// located (which disables host-key persistence rather than crashing).
func KnownHostsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "vps-manager", "known_hosts")
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

// Add inserts a new server and returns it with its generated ID populated, so
// callers (e.g. creating a project with inline server details) can reference it.
func (s *Store) Add(v VPS) (VPS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = newID()
	}
	if v.Port == 0 {
		v.Port = 22
	}
	s.data.VPSes = append(s.data.VPSes, v)
	return v, s.save()
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

// ───────── Deployments ─────────

func (s *Store) ListDeployments() []Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Deployment, len(s.data.Deployments))
	copy(out, s.data.Deployments)
	return out
}

func (s *Store) GetDeployment(id string) (Deployment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.data.Deployments {
		if d.ID == id {
			return d, true
		}
	}
	return Deployment{}, false
}

// SaveDeployment inserts (empty ID) or updates (existing ID) and returns the
// stored value, ID included.
func (s *Store) SaveDeployment(d Deployment) (Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.Branch == "" {
		d.Branch = "main"
	}
	if d.ID == "" {
		d.ID = "dep_" + hex.EncodeToString(randBytes())
		s.data.Deployments = append(s.data.Deployments, d)
		return d, s.save()
	}
	for i, existing := range s.data.Deployments {
		if existing.ID == d.ID {
			s.data.Deployments[i] = d
			return d, s.save()
		}
	}
	return Deployment{}, fmt.Errorf("deployment %s not found", d.ID)
}

func (s *Store) DeleteDeployment(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.data.Deployments {
		if d.ID == id {
			s.data.Deployments = append(s.data.Deployments[:i], s.data.Deployments[i+1:]...)
			return s.save()
		}
	}
	return nil
}

// SetDeployStatus records the outcome of a deploy run.
func (s *Store) SetDeployStatus(id, status, commit, when string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Deployments {
		if s.data.Deployments[i].ID == id {
			s.data.Deployments[i].LastStatus = status
			if commit != "" {
				s.data.Deployments[i].LastCommit = commit
			}
			if when != "" {
				s.data.Deployments[i].LastDeploy = when
			}
			_ = s.save()
			return
		}
	}
}

// ───────── Projects ─────────

func (s *Store) ListProjects() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, len(s.data.Projects))
	copy(out, s.data.Projects)
	return out
}

// SaveProject inserts (empty ID) or updates (existing ID) and returns the stored
// value with its ID populated.
func (s *Store) SaveProject(p Project) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = "proj_" + hex.EncodeToString(randBytes())
		s.data.Projects = append(s.data.Projects, p)
		return p, s.save()
	}
	for i, existing := range s.data.Projects {
		if existing.ID == p.ID {
			s.data.Projects[i] = p
			return p, s.save()
		}
	}
	return Project{}, fmt.Errorf("project %s not found", p.ID)
}

func (s *Store) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.data.Projects {
		if p.ID == id {
			s.data.Projects = append(s.data.Projects[:i], s.data.Projects[i+1:]...)
			return s.save()
		}
	}
	return nil
}

func newID() string {
	return "vps_" + hex.EncodeToString(randBytes())
}

func randBytes() []byte {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return b[:]
}
