package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// bridgeTokenFile is the name, inside the store dir, of the file holding the
// shared secret REST clients must send in the X-Bridge-Token header.
const bridgeTokenFile = "bridge_token"

// bridgeTokenHeader is the request header carrying the token.
const bridgeTokenHeader = "X-Bridge-Token"

// bridgeTokenBytes is the amount of randomness in a token (hex-encoded on disk).
const bridgeTokenBytes = 32

var errInvalidToken = errors.New("invalid bridge token file")

// ensureBridgeToken returns the API token stored in storeDir, creating it
// first if it is missing or malformed. The file is written to a temp file
// (0600 from os.CreateTemp) and renamed into place, so readers never see a
// partial token.
func ensureBridgeToken(storeDir string) (string, error) {
	path := filepath.Join(storeDir, bridgeTokenFile)
	tok, err := readBridgeToken(path)
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("restrict %s: %w", path, err)
		}
		return tok, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errInvalidToken) {
		return "", err
	}

	buf := make([]byte, bridgeTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate bridge token: %w", err)
	}
	tok = hex.EncodeToString(buf)

	tmp, err := os.CreateTemp(storeDir, ".bridge_token-*")
	if err != nil {
		return "", fmt.Errorf("create bridge token: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("create bridge token: %w", err)
	}
	if _, err := tmp.WriteString(tok); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write bridge token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write bridge token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write bridge token: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("install bridge token: %w", err)
	}
	return tok, nil
}

// readBridgeToken reads a token file, returning errInvalidToken if it does
// not hold exactly one hex token of the expected length.
func readBridgeToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if len(tok) != 2*bridgeTokenBytes {
		return "", errInvalidToken
	}
	if _, err := hex.DecodeString(tok); err != nil {
		return "", errInvalidToken
	}
	return tok, nil
}

// guard wraps the API mux with the request checks every /api/* call must
// pass, in this order:
//
//  1. Host must be 127.0.0.1:<port> or localhost:<port> (403). This blocks
//     DNS rebinding, where a page on another origin resolves its own name
//     to 127.0.0.1.
//  2. X-Bridge-Token must match the token file (401), compared in constant
//     time.
//  3. POST bodies must be declared application/json (415). Browsers can send
//     text/plain and form bodies cross-origin without a preflight; they
//     cannot send application/json or custom headers without one.
func (s *apiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "Forbidden host", http.StatusForbidden)
			return
		}
		got := r.Header.Get(bridgeTokenHeader)
		if s.token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "Missing or invalid "+bridgeTokenHeader, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost reports whether a request's Host header names the loopback
// listener on s.port.
func (s *apiServer) allowedHost(host string) bool {
	if s.port <= 0 {
		return false
	}
	port := fmt.Sprintf(":%d", s.port)
	return host == "127.0.0.1"+port || strings.EqualFold(host, "localhost"+port)
}
