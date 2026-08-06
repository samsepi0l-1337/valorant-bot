package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

// Boxer encrypts and decrypts data with AES-GCM using a key derived from a secret.
type Boxer struct {
	gcm cipher.AEAD
}

// NewBoxer derives a 32-byte AES key from secret via SHA-256 and builds an AES-GCM boxer.
func NewBoxer(secret string) (*Boxer, error) {
	if secret == "" {
		return nil, fmt.Errorf("secret is required")
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Boxer{gcm: gcm}, nil
}

// Encrypt seals plaintext with a random nonce prepended to the ciphertext.
func (b *Boxer) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return b.gcm.Seal(nonce, nonce, plain, nil), nil
}

// Decrypt opens ciphertext produced by Encrypt (nonce || sealed).
func (b *Boxer) Decrypt(cipherText []byte) ([]byte, error) {
	ns := b.gcm.NonceSize()
	if len(cipherText) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, sealed := cipherText[:ns], cipherText[ns:]
	plain, err := b.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}
