package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// outboxDirName is the default directory, inside the store dir, that files
// must be placed in before they can be sent.
const outboxDirName = "outbox"

// sendAllowedDirsEnv lists extra directories (os.PathListSeparator-separated)
// that files may be sent from.
const sendAllowedDirsEnv = "WHATSAPP_SEND_ALLOWED_DIRS"

// sendAllowedDirs creates <storeDir>/outbox (0700) and returns it plus the
// directories listed in WHATSAPP_SEND_ALLOWED_DIRS, all made absolute with
// symlinks resolved. Extra entries that are empty or not existing
// directories are skipped with a warning.
func sendAllowedDirs(storeDir string) ([]string, error) {
	outbox := filepath.Join(storeDir, outboxDirName)
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		return nil, fmt.Errorf("create outbox: %w", err)
	}
	if err := os.Chmod(outbox, 0o700); err != nil {
		return nil, fmt.Errorf("restrict outbox: %w", err)
	}
	resolved, err := resolveDir(outbox)
	if err != nil {
		return nil, fmt.Errorf("resolve outbox: %w", err)
	}
	dirs := []string{resolved}

	for _, d := range filepath.SplitList(os.Getenv(sendAllowedDirsEnv)) {
		if strings.TrimSpace(d) == "" {
			continue
		}
		r, err := resolveDir(d)
		if err != nil {
			fmt.Printf("Warning: ignoring %s entry %q: %v\n", sendAllowedDirsEnv, d, err)
			continue
		}
		dirs = append(dirs, r)
	}
	return dirs, nil
}

// resolveDir returns dir as an absolute path with symlinks resolved, and
// checks it is a directory.
func resolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return real, nil
}

// resolveSendPath checks that path names a regular file inside one of
// allowedDirs once made absolute and with every symlink resolved (so ".."
// and links pointing out of the directory are caught), and returns that
// resolved path. The caller should read the returned path, not the input.
func resolveSendPath(path string, allowedDirs []string) (string, error) {
	if path == "" {
		return "", errors.New("media_path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid media_path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("media file not found: %s", path)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("media file not found: %s", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("media_path is not a regular file: %s", path)
	}

	for _, dir := range allowedDirs {
		rd, err := resolveDir(dir)
		if err != nil {
			continue
		}
		prefix := rd
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(real, prefix) {
			return real, nil
		}
	}

	hint := "the bridge's store/outbox directory"
	if len(allowedDirs) > 0 {
		hint = allowedDirs[0]
	}
	return "", fmt.Errorf("media_path %s is outside the directories files may be sent from; copy it into %s first (or add its directory to %s)", path, hint, sendAllowedDirsEnv)
}

// sendFileOpenHook, if set, runs between the allowlist check and opening the
// file. Tests use it to swap the file mid-check. Nil in production.
var sendFileOpenHook func()

// openSendFile checks path against allowedDirs and opens it.
// TODO(nit): currently re-opens by path after the check (TOCTOU).
func openSendFile(path string, allowedDirs []string) (*os.File, string, error) {
	real, err := resolveSendPath(path, allowedDirs)
	if err != nil {
		return nil, "", err
	}
	if sendFileOpenHook != nil {
		sendFileOpenHook()
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", err
	}
	return f, real, nil
}
