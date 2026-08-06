package crypto

import (
	"bytes"
	"testing"
)

func TestNewBoxer_EmptySecret(t *testing.T) {
	_, err := NewBoxer("")
	if err == nil {
		t.Fatal("expected error for empty secret")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	b, err := NewBoxer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewBoxer: %v", err)
	}

	plain := []byte(`{"ssid":"abc","tdid":"xyz"}`)
	cipher, err := b.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(cipher, plain) {
		t.Fatal("ciphertext must differ from plaintext")
	}

	got, err := b.Decrypt(cipher)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("got %q, want %q", got, plain)
	}
}

func TestEncrypt_ProducesUniqueCiphertexts(t *testing.T) {
	b, err := NewBoxer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewBoxer: %v", err)
	}
	plain := []byte("same-payload")
	c1, err := b.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := b.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(c1, c2) {
		t.Fatal("expected different ciphertexts due to random nonce")
	}
}

func TestDecrypt_Tampered(t *testing.T) {
	b, err := NewBoxer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewBoxer: %v", err)
	}
	cipher, err := b.Encrypt([]byte("secret-cookies"))
	if err != nil {
		t.Fatal(err)
	}
	cipher[len(cipher)-1] ^= 0xff
	_, err = b.Decrypt(cipher)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	b, err := NewBoxer("short-but-hashed-is-fine-secret!!")
	if err != nil {
		t.Fatalf("NewBoxer: %v", err)
	}
	_, err = b.Decrypt([]byte("tiny"))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDifferentSecrets_CannotDecrypt(t *testing.T) {
	b1, err := NewBoxer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := NewBoxer("fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := b1.Encrypt([]byte("cookies"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = b2.Decrypt(cipher)
	if err == nil {
		t.Fatal("expected decrypt failure with different secret")
	}
}
