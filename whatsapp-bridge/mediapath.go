package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// safeMediaFilename reduces a sender-provided filename to a safe base name.
// TODO(#3): not implemented yet.
func safeMediaFilename(name string) (string, bool) {
	return name, name != ""
}

// mediaLocalPath returns where media for chatJID/filename is saved under
// storeDir. TODO(#3): not implemented yet (mirrors the old unsafe logic and
// does not create directories).
func mediaLocalPath(storeDir, chatJID, filename string) (string, error) {
	chatDir := fmt.Sprintf("%s/%s", storeDir, strings.ReplaceAll(chatJID, ":", "_"))
	return filepath.Abs(fmt.Sprintf("%s/%s", chatDir, filename))
}

// saveMediaFile writes downloaded media. TODO(#3): not implemented yet.
func saveMediaFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
