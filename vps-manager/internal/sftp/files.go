package sftp

import (
	"fmt"
	"io"
	"os"
	"path"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	IsDir   bool   `json:"isDir"`
	ModTime int64  `json:"modTime"` // unix seconds
}

func newClient(sshClient *ssh.Client) (*sftp.Client, error) {
	c, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp init: %w", err)
	}
	return c, nil
}

// List returns the entries of the remote directory `dir`. Remote paths are
// always POSIX-style, so we use the `path` package, not `filepath`.
func List(sshClient *ssh.Client, dir string) ([]FileInfo, error) {
	c, err := newClient(sshClient)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	entries, err := c.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	files := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		files = append(files, FileInfo{
			Name:    e.Name(),
			Path:    path.Join(dir, e.Name()),
			Size:    e.Size(),
			Mode:    e.Mode().String(),
			IsDir:   e.IsDir(),
			ModTime: e.ModTime().Unix(),
		})
	}
	return files, nil
}

func Download(sshClient *ssh.Client, remotePath, localPath string) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()

	src, err := c.Open(remotePath)
	if err != nil {
		return fmt.Errorf("open remote: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create local: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

func Upload(sshClient *ssh.Client, localPath, remotePath string) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()

	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local: %w", err)
	}
	defer src.Close()

	dst, err := c.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create remote: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// Delete removes a file or empty directory. Use with care.
func Delete(sshClient *ssh.Client, p string) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()

	stat, err := c.Stat(p)
	if err != nil {
		return err
	}
	if stat.IsDir() {
		return c.RemoveDirectory(p)
	}
	return c.Remove(p)
}
