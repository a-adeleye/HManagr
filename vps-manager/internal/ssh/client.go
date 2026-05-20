package ssh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Connection wraps a live *ssh.Client tied to a particular VPS ID.
type Connection struct {
	Client *ssh.Client
	host   string
}

// Pool keeps a map of live SSH connections keyed by VPS ID so we don't
// re-dial for every action (a UI doing per-action connects would feel laggy).
type Pool struct {
	mu    sync.Mutex
	conns map[string]*Connection
}

func NewPool() *Pool {
	return &Pool{conns: make(map[string]*Connection)}
}

type ConnectOptions struct {
	ID       string
	Host     string
	Port     int
	User     string
	AuthType string // "key" or "password"
	KeyPath  string
	Password string
}

// Connect dials the VPS and stores the live client. If we already have a
// live connection for this ID, Connect is a no-op.
func (p *Pool) Connect(opts ConnectOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.conns[opts.ID]; ok {
		// Cheap liveness check; OpenSSH treats unknown requests as no-op.
		if _, _, err := c.Client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return nil
		}
		_ = c.Client.Close()
		delete(p.conns, opts.ID)
	}

	var authMethods []ssh.AuthMethod
	switch opts.AuthType {
	case "key", "":
		path := expandHome(opts.KeyPath)
		key, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read key %s: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return fmt.Errorf("parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	case "password":
		authMethods = append(authMethods, ssh.Password(opts.Password))
	default:
		return fmt.Errorf("unknown auth type: %s", opts.AuthType)
	}

	port := opts.Port
	if port == 0 {
		port = 22
	}

	cfg := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: known_hosts verification
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(opts.Host, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	p.conns[opts.ID] = &Connection{Client: client, host: addr}
	return nil
}

func (p *Pool) Get(id string) (*Connection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.conns[id]
	if !ok {
		return nil, fmt.Errorf("not connected to %s", id)
	}
	return c, nil
}

func (p *Pool) Disconnect(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.conns[id]
	if !ok {
		return nil
	}
	delete(p.conns, id)
	return c.Client.Close()
}

func (p *Pool) IsConnected(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.conns[id]
	return ok
}

// Close terminates all pooled connections (called on app shutdown).
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Client.Close()
	}
	p.conns = make(map[string]*Connection)
}

// ExecResult captures the output of a single SSH command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Exec runs a command via a fresh SSH session and returns its full output.
// It honours ctx cancellation by sending SIGKILL to the remote process.
func (c *Connection) Exec(ctx context.Context, cmd string) (*ExecResult, error) {
	session, err := c.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Start(cmd); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		<-done
		return &ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: -1,
		}, ctx.Err()

	case err := <-done:
		res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if err == nil {
			return res, nil
		}
		if exitErr, ok := err.(*ssh.ExitError); ok {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil // non-zero exits aren't transport errors
		}
		return res, err
	}
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + p[1:]
}
