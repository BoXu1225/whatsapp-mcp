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
// directories listed in WHATSAPP_SEND_ALLOWED_DIRS, with symlinks resolved.
// Extra entries must be absolute; relative ones and ones that are not
// existing directories are skipped with a warning (empty ones silently).
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
		if !filepath.IsAbs(d) {
			fmt.Printf("Warning: ignoring %s entry %q: must be an absolute path\n", sendAllowedDirsEnv, d)
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
// resolved path. To read the file, use openSendFile, which also guards
// against the file being swapped after the check.
func resolveSendPath(path string, allowedDirs []string) (string, error) {
	real, _, err := checkSendPath(path, allowedDirs)
	return real, err
}

// checkSendPath is resolveSendPath that also returns the Lstat of the
// resolved path, identifying the exact file that passed the check.
func checkSendPath(path string, allowedDirs []string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, errors.New("media_path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("invalid media_path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("media file not found: %s", path)
	}
	// real has no symlinks left, so Lstat describes the file itself.
	info, err := os.Lstat(real)
	if err != nil {
		return "", nil, fmt.Errorf("media file not found: %s", path)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("media_path is not a regular file: %s", path)
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
			return real, info, nil
		}
	}

	hint := "the bridge's store/outbox directory"
	if len(allowedDirs) > 0 {
		hint = allowedDirs[0]
	}
	return "", nil, fmt.Errorf("media_path %s is outside the directories files may be sent from; copy it into %s first (or add its directory to %s)", path, hint, sendAllowedDirsEnv)
}

// sendFileOpenHook, if set, runs between the allowlist check and opening the
// file. Tests use it to swap the file mid-check. Nil in production.
var sendFileOpenHook func()

// errSendFileChanged is returned when the file changed between the check and
// the open.
var errSendFileChanged = errors.New("media file changed while it was being checked; not sending it")

// openSendFile checks path against allowedDirs and returns an open handle to
// the file that passed the check, plus its resolved path. After opening it
// fstats the handle and requires it to be a regular file and the same file
// (os.SameFile) as the one checked, and requires the resolved path to still
// resolve to itself and name that file. Read the media from the handle, never
// by path again.
func openSendFile(path string, allowedDirs []string) (*os.File, string, error) {
	real, checked, err := checkSendPath(path, allowedDirs)
	if err != nil {
		return nil, "", err
	}
	if sendFileOpenHook != nil {
		sendFileOpenHook()
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", fmt.Errorf("open media file: %w", err)
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(opened, checked) {
		f.Close()
		return nil, "", errSendFileChanged
	}
	again, err := filepath.EvalSymlinks(real)
	if err != nil || again != real {
		f.Close()
		return nil, "", errSendFileChanged
	}
	if now, err := os.Lstat(real); err != nil || !os.SameFile(now, opened) {
		f.Close()
		return nil, "", errSendFileChanged
	}
	return f, real, nil
}
