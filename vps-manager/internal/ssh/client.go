package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// ExecWithStdin behaves like Exec but pipes the given string into the remote
// process's stdin. Used for `sudo -S` so the password never appears on the
// command line (no /proc/<pid>/cmdline leak, no audit-log capture).
func (c *Connection) ExecWithStdin(ctx context.Context, cmd, stdin string) (*ExecResult, error) {
	session, err := c.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := session.Start(cmd); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	go func() {
		_, _ = io.WriteString(stdinPipe, stdin)
		_ = stdinPipe.Close()
	}()

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		<-done
		return &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}, ctx.Err()
	case err := <-done:
		res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if err == nil {
			return res, nil
		}
		if exitErr, ok := err.(*ssh.ExitError); ok {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		return res, err
	}
}

// ExecStream runs cmd and invokes onOutput with chunks of combined
// stdout+stderr as they arrive, instead of buffering until exit. Used for
// long-running commands whose progress the user wants to watch (docker builds,
// compose up). Returns the remote exit code.
func (c *Connection) ExecStream(ctx context.Context, cmd string, onOutput func(string)) (int, error) {
	session, err := c.Client.NewSession()
	if err != nil {
		return -1, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := session.Start(cmd); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	var outMu sync.Mutex
	emit := func(b []byte) {
		if len(b) == 0 || onOutput == nil {
			return
		}
		outMu.Lock()
		onOutput(string(b))
		outMu.Unlock()
	}
	var wg sync.WaitGroup
	for _, r := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			buf := make([]byte, 16*1024)
			for {
				n, readErr := r.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					emit(chunk)
				}
				if readErr != nil {
					return
				}
			}
		}(r)
	}

	done := make(chan error, 1)
	go func() {
		wg.Wait()
		done <- session.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		<-done
		return -1, ctx.Err()
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return exitErr.ExitStatus(), nil
		}
		return -1, err
	}
}

// Session is a live interactive shell backed by a PTY on the remote host.
// Unlike Exec (one-shot, stateless), a Session is long-lived: the remote shell
// keeps its working directory, environment, and any foreground program (psql,
// nano, top, …) alive across writes. Terminal output is streamed to the onData
// callback as it arrives.
type Session struct {
	sess   *ssh.Session
	stdin  io.WriteCloser
	mu     sync.Mutex
	closed bool
}

// Shell opens an interactive login shell with a PTY of the given size.
//
//   - onData is invoked from a background goroutine with chunks of raw terminal
//     output (merged stdout+stderr) as they arrive. It must not block for long.
//   - onClose is invoked once, after the remote shell exits and the output
//     stream drains. It may be nil.
func (c *Connection) Shell(cols, rows int, onData func([]byte), onClose func()) (*Session, error) {
	sess, err := c.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}

	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	// With a PTY, the remote merges stderr onto the terminal, so reading stdout
	// alone gives us everything the user would see on a real terminal.
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := sess.Shell(); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}

	s := &Session{sess: sess, stdin: stdin}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 && onData != nil {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				onData(chunk)
			}
			if readErr != nil {
				break
			}
		}
		_ = sess.Wait()
		s.markClosed()
		if onClose != nil {
			onClose()
		}
	}()

	return s, nil
}

// Write sends raw bytes (keystrokes, pasted text, control sequences) to the
// shell's stdin.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("session closed")
	}
	_, err := s.stdin.Write(p)
	return err
}

// Resize informs the remote PTY of a new terminal size so full-screen programs
// redraw correctly.
func (s *Session) Resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.sess.WindowChange(rows, cols)
}

// Close terminates the shell. Safe to call more than once.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.stdin.Close()
	return s.sess.Close()
}

func (s *Session) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
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
