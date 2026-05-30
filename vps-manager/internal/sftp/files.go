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

const maxEditBytes = 512 * 1024 // 512 KB

func ReadFile(sshClient *ssh.Client, remotePath string) (string, error) {
	c, err := newClient(sshClient)
	if err != nil {
		return "", err
	}
	defer c.Close()

	info, err := c.Stat(remotePath)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if info.Size() > maxEditBytes {
		return "", fmt.Errorf("file is too large to edit in-app (%.1f MB); use Download instead", float64(info.Size())/1024/1024)
	}

	f, err := c.Open(remotePath)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return string(b), nil
}

func WriteFile(sshClient *ssh.Client, remotePath, content string) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()

	f, err := c.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()

	_, err = f.Write([]byte(content))
	return err
}

// Delete removes a file or empty directory. Use with care.
// PathStat is the metadata the permissions UI needs about a path: the four-
// digit octal mode (perm bits + setuid/setgid/sticky), the numeric owner/group,
// and whether the path is a directory.
type PathStat struct {
	Mode  string `json:"mode"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	IsDir bool   `json:"isDir"`
}

// Stat returns mode, uid and gid for p. Names are not resolved here — that
// requires running a shell command, which is the App layer's job.
func Stat(sshClient *ssh.Client, p string) (PathStat, error) {
	c, err := newClient(sshClient)
	if err != nil {
		return PathStat{}, err
	}
	defer c.Close()
	info, err := c.Stat(p)
	if err != nil {
		return PathStat{}, err
	}
	ps := PathStat{
		Mode:  fmt.Sprintf("%04o", info.Mode().Perm()),
		IsDir: info.IsDir(),
	}
	// Prefer raw sftp.FileStat when available: it carries the setuid/setgid/
	// sticky bits the os.FileMode permission mask drops, plus uid/gid.
	if sys, ok := info.Sys().(*sftp.FileStat); ok {
		ps.Mode = fmt.Sprintf("%04o", sys.Mode&07777)
		ps.UID = int(sys.UID)
		ps.GID = int(sys.GID)
	}
	return ps, nil
}

// Chmod changes the permission bits of p. mode is the full mode value
// including any high bits (setuid/setgid/sticky).
func Chmod(sshClient *ssh.Client, p string, mode os.FileMode) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Chmod(p, mode)
}

// Chown changes the numeric owner/group of p. Use -1 to leave one unchanged.
func Chown(sshClient *ssh.Client, p string, uid, gid int) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()
	// pkg/sftp.Chown doesn't accept -1; fetch current to fill missing side.
	if uid < 0 || gid < 0 {
		info, err := c.Stat(p)
		if err != nil {
			return err
		}
		if sys, ok := info.Sys().(*sftp.FileStat); ok {
			if uid < 0 {
				uid = int(sys.UID)
			}
			if gid < 0 {
				gid = int(sys.GID)
			}
		}
	}
	return c.Chown(p, uid, gid)
}

// Mkdir creates the directory at p, creating any missing parents (mkdir -p
// semantics). Returns an error if a non-directory already exists at the path.
func Mkdir(sshClient *ssh.Client, p string) error {
	c, err := newClient(sshClient)
	if err != nil {
		return err
	}
	defer c.Close()

	if stat, err := c.Stat(p); err == nil {
		if stat.IsDir() {
			return nil
		}
		return fmt.Errorf("%s exists and is not a directory", p)
	}
	return c.MkdirAll(p)
}

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
