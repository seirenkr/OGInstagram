package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	offloadSignatureVersion = "2"
	offloadSigningKeySize   = 32

	offloadCapabilityTTL = 14 * 24 * time.Hour
)

var offloadKeyIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)

type offloadSigner struct {
	active string
	keys   map[string][]byte
}

type offloadSigningConfig struct {
	Active string            `json:"active"`
	Keys   map[string]string `json:"keys"`
}

// Without OFFLOAD_SIGNING_KEYS, the keyring generated on first start persists in
// DATA_DIR, so issued links stay valid across restarts and deploys.
func loadOrCreateOffloadKeys(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "offload-signing-keys.json")
	if b, err := os.ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
		return string(b), err
	}
	key := make([]byte, offloadSigningKeySize)
	rand.Read(key)
	raw := `{"active":"local","keys":{"local":"` + base64.RawURLEncoding.EncodeToString(key) + `"}}`
	// Write a complete 0600 temp file, then link it into place: a crash never
	// leaves a partial keyring, and a concurrent creator's keyring wins intact.
	tmp, err := os.CreateTemp(dataDir, ".offload-signing-keys-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(raw); err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return "", err
	}
	if err = os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	return raw, err
}

func parseOffloadSigner(raw string) (offloadSigner, error) {
	var config offloadSigningConfig
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return offloadSigner{}, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return offloadSigner{}, errors.New("multiple JSON values")
	}
	if !offloadKeyIDRE.MatchString(config.Active) {
		return offloadSigner{}, errors.New("active key ID must match [A-Za-z0-9_-]{1,16}")
	}
	if len(config.Keys) == 0 {
		return offloadSigner{}, errors.New("at least one key is required")
	}

	keys := make(map[string][]byte, len(config.Keys))
	for id, encoded := range config.Keys {
		if !offloadKeyIDRE.MatchString(id) {
			return offloadSigner{}, fmt.Errorf("key ID %q must match [A-Za-z0-9_-]{1,16}", id)
		}
		key, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(key) != offloadSigningKeySize || base64.RawURLEncoding.EncodeToString(key) != encoded {
			return offloadSigner{}, fmt.Errorf("key %q must be a base64url-encoded 32-byte value", id)
		}
		keys[id] = key
	}
	if _, ok := keys[config.Active]; !ok {
		return offloadSigner{}, errors.New("active key is missing from keys")
	}
	return offloadSigner{active: config.Active, keys: keys}, nil
}

// offloadMAC is shared by signing and authorize, so both hash the same input.
func offloadMAC(key []byte, keyID, path string, expires int64) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("oginstagram-offload-capability\n" +
		"v=" + offloadSignatureVersion + "\n" +
		"kid=" + keyID + "\n" +
		"exp=" + strconv.FormatInt(expires, 10) + "\n" +
		"path=" + path + "\n"))
	return mac.Sum(nil)
}

func (s offloadSigner) signature(path string, expires int64) string {
	key := s.keys[s.active]
	if len(key) != offloadSigningKeySize {
		panic("offload signer is not configured")
	}
	return base64.RawURLEncoding.EncodeToString(offloadMAC(key, s.active, path, expires))
}

func (s offloadSigner) url(baseURL, path string, thumbnail bool) string {
	expires := time.Now().Add(offloadCapabilityTTL).Unix()
	query := "v=" + offloadSignatureVersion + "&kid=" + s.active +
		"&exp=" + strconv.FormatInt(expires, 10) + "&sig=" + s.signature(path, expires)
	if thumbnail {
		query = "thumbnail=1&" + query
	}
	return strings.TrimRight(baseURL, "/") + path + "?" + query
}
