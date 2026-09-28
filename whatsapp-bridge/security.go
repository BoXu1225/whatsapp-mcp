package main

import "net/http"

// bridgeTokenFile is the name, inside the store dir, of the file holding the
// shared secret REST clients must send in the X-Bridge-Token header.
const bridgeTokenFile = "bridge_token"

// ensureBridgeToken returns the API token stored in storeDir, creating it
// first if needed. TODO(#1): not implemented yet.
func ensureBridgeToken(storeDir string) (string, error) {
	return "", nil
}

// guard wraps the API mux with request checks. TODO(#1): not implemented yet.
func (s *apiServer) guard(next http.Handler) http.Handler {
	return next
}
