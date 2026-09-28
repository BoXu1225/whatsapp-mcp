package main

// debugLogging turns on logging of message content, outbound text and file
// paths. Set by the -debug flag. TODO(#4): not used yet.
var debugLogging bool

// bridgeFlags are the command-line options of the bridge.
type bridgeFlags struct {
	debug bool
}

// parseFlags parses the bridge's command-line options.
// TODO(#4): not implemented yet.
func parseFlags(args []string) (bridgeFlags, error) {
	return bridgeFlags{}, nil
}

// hardenStorePermissions restricts the store dir and its secrets to the
// owner. TODO(#4): not implemented yet.
func hardenStorePermissions(storeDir string) error {
	return nil
}
