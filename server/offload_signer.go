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
	"net/url"
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

	offloadCapabilityTTL = 30 * 24 * time.Hour
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

// backupOffloadKeys copies the generated keyring, if any, into a backup so
// restored data keeps already shared links valid.
func backupOffloadKeys(dataDir, destinationDir string) error {
	b, err := os.ReadFile(filepath.Join(dataDir, "offload-signing-keys.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destinationDir, "offload-signing-keys.json"), b, 0o600)
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

// ponytail: the 78-byte capability query costs about 29% of Discord's 3000-byte
// component budget; at the next format change, truncate the MAC to 16 bytes and
// fold v into kid.
func (s offloadSigner) url(baseURL, path string, thumbnail bool) string {
	// Hourly expiries give every render in the same hour one URL, and so one
	// edge cache entry.
	expires := time.Now().Add(offloadCapabilityTTL).Truncate(time.Hour).Unix()
	query := "v=" + offloadSignatureVersion + "&kid=" + s.active +
		"&exp=" + strconv.FormatInt(expires, 10) + "&sig=" + s.signature(path, expires)
	if thumbnail {
		query = "thumbnail=1&" + query
	}
	return strings.TrimRight(baseURL, "/") + path + "?" + query
}

func (s offloadSigner) authorize(u *url.URL, now time.Time) bool {
	path, ok := canonicalOffloadPath(u)
	if !ok {
		return false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	// Only the exact query url emits: any extra, reordered or re-encoded
	// parameter would be a fresh edge cache key for the same capability.
	v, kid, exp, sig := values.Get("v"), values.Get("kid"), values.Get("exp"), values.Get("sig")
	canonical := "v=" + v + "&kid=" + kid + "&exp=" + exp + "&sig=" + sig
	if u.RawQuery != canonical && u.RawQuery != "thumbnail=1&"+canonical {
		return false
	}
	if v != offloadSignatureVersion {
		return false
	}
	key := s.keys[kid]
	if len(key) != offloadSigningKeySize {
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || strconv.FormatInt(expires, 10) != exp || expires <= now.Unix() || expires > now.Unix()+int64(offloadCapabilityTTL/time.Second) {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != sig {
		return false
	}
	return hmac.Equal(signature, offloadMAC(key, kid, path, expires))
}
