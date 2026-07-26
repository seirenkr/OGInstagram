package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	offloadSignatureVersion = "2"
	offloadSigningKeySize   = 32

	offloadCapabilityTTL = 14*24*time.Hour - time.Minute
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

func offloadSignatureInput(keyID, path string, expires int64) []byte {
	return []byte("oginstagram-offload-capability\n" +
		"v=" + offloadSignatureVersion + "\n" +
		"kid=" + keyID + "\n" +
		"exp=" + strconv.FormatInt(expires, 10) + "\n" +
		"path=" + path + "\n")
}

func (s offloadSigner) signature(path string, expires int64) string {
	key := s.keys[s.active]
	if len(key) != offloadSigningKeySize {
		panic("offload signer is not configured")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(offloadSignatureInput(s.active, path, expires))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s offloadSigner) url(baseURL, path string, thumbnail bool) string {
	query := ""
	if thumbnail {
		query = "thumbnail=1"
	}
	expires := time.Now().Add(offloadCapabilityTTL).Unix()
	signature := s.signature(path, expires)
	if query != "" {
		query += "&"
	}
	query += "v=" + offloadSignatureVersion + "&kid=" + s.active +
		"&exp=" + strconv.FormatInt(expires, 10) + "&sig=" + signature
	return strings.TrimRight(baseURL, "/") + path + "?" + query
}
