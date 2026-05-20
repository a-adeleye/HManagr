// Package docker runs docker CLI commands over an SSH connection and parses
// the results. Using `docker --format '{{json .}}'` is much simpler and more
// portable than talking to the Docker socket directly (which would need
// tunneling).
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

type Container struct {
	ID     string `json:"id"`
	Names  string `json:"names"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
	Ports  string `json:"ports"`
}

// dockerPS is the shape of a single line of `docker ps --format '{{json .}}'`.
type dockerPS struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	Status string `json:"Status"`
	State  string `json:"State"`
	Ports  string `json:"Ports"`
}

func List(ctx context.Context, conn *sshpkg.Connection) ([]Container, error) {
	res, err := conn.Exec(ctx, `docker ps -a --format '{{json .}}'`)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var containers []Container
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		var raw dockerPS
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // skip malformed line rather than failing the whole list
		}
		containers = append(containers, Container{
			ID:     raw.ID,
			Names:  raw.Names,
			Image:  raw.Image,
			Status: raw.Status,
			State:  raw.State,
			Ports:  raw.Ports,
		})
	}
	return containers, nil
}

func Restart(ctx context.Context, conn *sshpkg.Connection, id string) error {
	return simpleAction(ctx, conn, "restart", id)
}

func Stop(ctx context.Context, conn *sshpkg.Connection, id string) error {
	return simpleAction(ctx, conn, "stop", id)
}

func Start(ctx context.Context, conn *sshpkg.Connection, id string) error {
	return simpleAction(ctx, conn, "start", id)
}

func simpleAction(ctx context.Context, conn *sshpkg.Connection, action, id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid container id")
	}
	res, err := conn.Exec(ctx, fmt.Sprintf("docker %s %s", action, id))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker %s (exit %d): %s", action, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func Logs(ctx context.Context, conn *sshpkg.Connection, id string, tail int) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid container id")
	}
	if tail <= 0 {
		tail = 200
	}
	cmd := fmt.Sprintf("docker logs --tail %d %s 2>&1", tail, id)
	res, err := conn.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// Exec runs `cmd` inside `containerID` via `docker exec sh -c`.
func Exec(ctx context.Context, conn *sshpkg.Connection, containerID, cmd string) (*sshpkg.ExecResult, error) {
	if !validID(containerID) {
		return nil, fmt.Errorf("invalid container id")
	}
	// Single-quote the inner command and escape single quotes.
	escaped := strings.ReplaceAll(cmd, "'", `'\''`)
	full := fmt.Sprintf("docker exec %s sh -c '%s'", containerID, escaped)
	return conn.Exec(ctx, full)
}

// validID rejects anything that doesn't look like a docker container id or
// name. This is a defense against injection since we interpolate it into the
// shell command. Docker IDs are hex; names are [a-zA-Z0-9][a-zA-Z0-9_.-]*.
func validID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
		if i == 0 && (r == '.' || r == '-') {
			return false
		}
	}
	return true
}
