/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package oauth

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// sealPrefix starts every sealed envelope, so a sealed DCR client_id is
// told apart from a CIMD URL or a pre-registered ID at a glance.
const sealPrefix = "kg1."

// Keyring seals and opens kode-gopher's envelopes (tokens, codes, state,
// DCR client IDs) with AES-256-GCM. The first key seals; every key
// opens, so a key is rotated by putting the new one first and dropping
// the old one once everything sealed with it has expired.
type Keyring struct {
	ids  []string
	aead map[string]cipher.AEAD
}

// NewKeyring builds a keyring from (id, 32-byte key) pairs, sealing key
// first.
func NewKeyring(keys ...KeyringKey) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("keyring: no keys")
	}
	k := &Keyring{aead: map[string]cipher.AEAD{}}
	for _, key := range keys {
		if key.ID == "" || strings.ContainsAny(key.ID, ". \t") {
			return nil, fmt.Errorf("keyring: bad key id %q", key.ID)
		}
		if len(key.Secret) != 32 {
			return nil, fmt.Errorf("keyring: key %q is %d bytes, want 32", key.ID, len(key.Secret))
		}
		if _, dup := k.aead[key.ID]; dup {
			return nil, fmt.Errorf("keyring: duplicate key id %q", key.ID)
		}
		block, err := aes.NewCipher(key.Secret)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.ids = append(k.ids, key.ID)
		k.aead[key.ID] = gcm
	}
	return k, nil
}

// KeyringKey is one sealing key.
type KeyringKey struct {
	ID     string
	Secret []byte
}

// LoadKeyring reads a keyring file: one key per line, "<id> <base64 of
// 32 random bytes>", sealing key first. Blank lines and # comments are
// skipped. Generate a key with `head -c32 /dev/urandom | base64`.
func LoadKeyring(path string) (*Keyring, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keyring: %w", err)
	}
	var keys []KeyringKey
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("keyring %s:%d: want \"<id> <base64 key>\"", path, n)
		}
		secret, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil {
			return nil, fmt.Errorf("keyring %s:%d: key isn't base64", path, n)
		}
		keys = append(keys, KeyringKey{ID: f[0], Secret: secret})
	}
	return NewKeyring(keys...)
}

// seal encrypts v's JSON. purpose is bound in as associated data, so an
// envelope sealed for one use (say, an authorization code) never opens
// as another (an access token).
func (k *Keyring) seal(purpose string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	id := k.ids[0]
	gcm := k.aead[id]
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, aad(purpose, id))
	return sealPrefix + id + "." + base64.RawURLEncoding.EncodeToString(ct), nil
}

// errBadEnvelope is every reason an envelope doesn't open. Callers map
// it to the OAuth error for their endpoint.
var errBadEnvelope = errors.New("invalid or tampered envelope")

// open decrypts an envelope sealed for purpose into v.
func (k *Keyring) open(purpose, s string, v any) error {
	rest, ok := strings.CutPrefix(s, sealPrefix)
	if !ok {
		return errBadEnvelope
	}
	id, body, ok := strings.Cut(rest, ".")
	if !ok {
		return errBadEnvelope
	}
	gcm, ok := k.aead[id]
	if !ok {
		return errBadEnvelope
	}
	ct, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(ct) < gcm.NonceSize() {
		return errBadEnvelope
	}
	plain, err := gcm.Open(nil, ct[:gcm.NonceSize()], ct[gcm.NonceSize():], aad(purpose, id))
	if err != nil {
		return errBadEnvelope
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return errBadEnvelope
	}
	return nil
}

func aad(purpose, id string) []byte {
	return []byte("kode-gopher/" + purpose + "/" + id)
}

// isSealed reports whether s looks like one of our envelopes.
func isSealed(s string) bool { return strings.HasPrefix(s, sealPrefix) }
