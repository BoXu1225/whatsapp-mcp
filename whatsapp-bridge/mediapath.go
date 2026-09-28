package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMediaFilenameBytes caps sanitised filenames (most filesystems allow 255).
const maxMediaFilenameBytes = 200

// safeMediaFilename reduces a sender-provided filename to its last path
// element, treating both / and \ as separators, replaces control characters
// with "_" and caps it at maxMediaFilenameBytes (keeping the extension). It
// reports false for names that are empty, ".", "..", a bare separator or
// contain NUL.
func safeMediaFilename(name string) (string, bool) {
	if strings.ContainsRune(name, 0) {
		return "", false
	}
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	switch base {
	case "", ".", "..", "/":
		return "", false
	}
	if strings.ContainsAny(base, `/\`) {
		return "", false
	}
	// Control characters (newlines, ESC, ...) would let a sender forge log
	// lines or terminal output wherever the name is printed.
	base = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, base)
	return truncateFilename(base, maxMediaFilenameBytes), true
}

// truncateFilename cuts name to at most max bytes, keeping a short extension
// and never splitting a UTF-8 sequence.
func truncateFilename(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > 16 || len(ext) == len(name) {
		ext = ""
	}
	stem := name[:len(name)-len(ext)]
	stem = stem[:max-len(ext)]
	for !utf8.ValidString(stem) {
		stem = stem[:len(stem)-1]
	}
	return stem + ext
}

// chatDirName turns a chat JID into a single directory name ("@" and "." are
// kept, ":" becomes "_") and rejects anything that isn't one plain element of
// the form user@server. Requiring the "@" keeps chat dirs from ever
// coinciding with the store's own entries (outbox, bridge_token, *.db).
func chatDirName(chatJID string) (string, error) {
	name := strings.ReplaceAll(chatJID, ":", "_")
	at := strings.Index(name, "@")
	switch {
	case at <= 0, at == len(name)-1:
		return "", fmt.Errorf("invalid chat JID %q", chatJID)
	case name == outboxDirName, name == bridgeTokenFile:
		return "", fmt.Errorf("invalid chat JID %q", chatJID)
	case strings.ContainsAny(name, `/\`), strings.ContainsRune(name, 0):
		return "", fmt.Errorf("invalid chat JID %q", chatJID)
	}
	return name, nil
}

// mediaLocalPath returns the absolute path media for chatJID/filename is
// saved at: <storeDir>/<chat>/<base name>, with storeDir's symlinks resolved.
// It creates the chat dir (0700) and refuses paths that would leave the
// store, a chat dir that is a symlink, or an existing file that is a symlink.
func mediaLocalPath(storeDir, chatJID, filename string) (string, error) {
	chat, err := chatDirName(chatJID)
	if err != nil {
		return "", err
	}
	name, ok := safeMediaFilename(filename)
	if !ok {
		return "", fmt.Errorf("invalid media filename %q", filename)
	}
	storeReal, err := resolveDir(storeDir)
	if err != nil {
		return "", fmt.Errorf("store dir: %w", err)
	}

	chatDir := filepath.Join(storeReal, chat)
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		return "", fmt.Errorf("create chat directory: %w", err)
	}
	if real, err := filepath.EvalSymlinks(chatDir); err != nil || real != chatDir {
		return "", fmt.Errorf("chat directory %s is not a plain directory inside the store", chatDir)
	}
	if err := os.Chmod(chatDir, 0o700); err != nil {
		return "", fmt.Errorf("restrict chat directory: %w", err)
	}

	p := filepath.Join(chatDir, name)
	if rel, err := filepath.Rel(storeReal, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("media path %s escapes the store", p)
	}
	if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("media path %s is a symlink", p)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return p, nil
}

// saveMediaFile writes downloaded media readable by the owner only.
func saveMediaFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
