package main

// outboxDirName is the default directory, inside the store dir, that files
// must be placed in before they can be sent.
const outboxDirName = "outbox"

// sendAllowedDirsEnv lists extra directories (os.PathListSeparator-separated)
// that files may be sent from.
const sendAllowedDirsEnv = "WHATSAPP_SEND_ALLOWED_DIRS"

// sendAllowedDirs returns the directories files may be sent from.
// TODO(#2): not implemented yet.
func sendAllowedDirs(storeDir string) ([]string, error) {
	return nil, nil
}

// resolveSendPath checks that path is a regular file inside one of
// allowedDirs. TODO(#2): not implemented yet.
func resolveSendPath(path string, allowedDirs []string) (string, error) {
	return path, nil
}
