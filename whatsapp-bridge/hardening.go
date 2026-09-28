package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// debugLogging turns on logging of message content, contact/chat names,
// outbound text and file paths. Off by default: at INFO the bridge logs
// message IDs, JIDs and lengths only. Set by the -debug flag.
var debugLogging bool

// bridgeFlags are the command-line options of the bridge.
type bridgeFlags struct {
	debug        bool
	purgeDeleted bool
}

// parseFlags parses the bridge's command-line options.
func parseFlags(args []string) (bridgeFlags, error) {
	fs := flag.NewFlagSet("whatsapp-bridge", flag.ContinueOnError)
	var f bridgeFlags
	fs.BoolVar(&f.debug, "debug", false, "log message content, chat names, outbound text and file paths (off by default)")
	fs.BoolVar(&f.purgeDeleted, "purge-deleted", false, "clear the stored text of messages deleted for everyone (default: keep it, marked deleted)")
	err := fs.Parse(args)
	return f, err
}

// debugPrintf prints only when -debug is on. Use it for anything carrying
// message content, names or file paths.
func debugPrintf(format string, args ...any) {
	if debugLogging {
		fmt.Printf(format, args...)
	}
}

// debugInfof logs at INFO only when -debug is on.
func debugInfof(logger waLog.Logger, format string, args ...any) {
	if debugLogging {
		logger.Infof(format, args...)
	}
}

// hardenStorePermissions restricts the store dir to the owner: the dir and
// its top-level subdirectories become 0700 and its top-level files (the
// SQLite DBs and their -wal/-shm/-journal files, bridge_token, logs) 0600.
// store/whatsapp.db holds the device's session keys.
func hardenStorePermissions(storeDir string) error {
	if err := os.Chmod(storeDir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(storeDir, e.Name())
		switch {
		case e.Type().IsRegular():
			err = os.Chmod(p, 0o600)
		case e.IsDir():
			err = os.Chmod(p, 0o700)
		default:
			continue // leave symlinks and special files alone
		}
		if err != nil {
			return err
		}
	}
	return nil
}
