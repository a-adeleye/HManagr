package local

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"vps-manager/internal/sftp"
)

// Local file operations mirror the sftp package's API shape but act on the host
// filesystem. Paths are returned with forward slashes (filepath.ToSlash) so the
// frontend — which joins and splits paths with "/" — works unchanged on
// Windows; the os package accepts forward-slash paths on Windows too, so input
// paths need no special handling.

const maxEditBytes = 512 * 1024 // 512 KB, matches sftp.ReadFile

// HomeDir returns the user's home directory (forward-slash), the natural
// starting point for the local file browser.
func HomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		if runtime.GOOS == "windows" {
			return "C:/"
		}
		return "/"
	}
	return filepath.ToSlash(h)
}

// List returns directory entries. Symlinks are reported by their own lstat info
// except we resolve IsDir against the target so folders are navigable.
func List(dir string) ([]sftp.FileInfo, error) {
	if dir == "" {
		dir = HomeDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	files := make([]sftp.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // entry vanished between ReadDir and stat; skip it
		}
		isDir := info.IsDir()
		size := info.Size()
		if info.Mode()&os.ModeSymlink != 0 {
			// Resolve symlink targets so directories remain clickable.
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				isDir = st.IsDir()
				size = st.Size()
			}
		}
		files = append(files, sftp.FileInfo{
			Name:    e.Name(),
			Path:    filepath.ToSlash(filepath.Join(dir, e.Name())),
			Size:    size,
			Mode:    info.Mode().String(),
			IsDir:   isDir,
			ModTime: info.ModTime().Unix(),
		})
	}
	return files, nil
}

// Download copies srcPath → dstPath (the "download" verb is kept for symmetry
// with the SSH path; locally it's a plain file copy to the chosen save path).
func Download(srcPath, dstPath string) error { return copyFile(srcPath, dstPath) }

// Upload copies srcPath → dstPath (symmetry with SSH upload; a local copy into
// the browsed directory).
func Upload(srcPath, dstPath string) error { return copyFile(srcPath, dstPath) }

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

func ReadFile(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	if info.Size() > maxEditBytes {
		return "", fmt.Errorf("file is too large to edit in-app (%.1f MB); use Download instead", float64(info.Size())/1024/1024)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func WriteFile(p, content string) error {
	// Preserve the existing mode when overwriting; default 0644 for new files.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(p, []byte(content), mode)
}

func Delete(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Remove(p) // non-recursive, matches sftp.Delete (empty dirs only)
	}
	return os.Remove(p)
}

func Mkdir(p string) error {
	if info, err := os.Stat(p); err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("%s exists and is not a directory", p)
	}
	return os.MkdirAll(p, 0o755)
}

// Stat returns mode/owner info. On Windows uid/gid have no meaning and come
// back as 0; the permissions modal still shows the octal mode.
func Stat(p string) (sftp.PathStat, error) {
	info, err := os.Stat(p)
	if err != nil {
		return sftp.PathStat{}, err
	}
	return sftp.PathStat{
		Mode:  fmt.Sprintf("%04o", info.Mode().Perm()),
		IsDir: info.IsDir(),
	}, nil
}

// Chmod applies an octal mode. On Windows only the read-only bit is honored by
// the OS, but the call is harmless and lets the same UI path work.
func Chmod(p string, mode os.FileMode) error {
	return os.Chmod(p, mode)
}
