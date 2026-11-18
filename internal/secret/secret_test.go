package secret

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	ct, err := Encrypt(key, "sk-upstream")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("sk-upstream")) {
		t.Fatal("plaintext leaked")
	}
	pt, err := Decrypt(key, ct)
	if err != nil || pt != "sk-upstream" {
		t.Fatalf("pt=%q err=%v", pt, err)
	}
	if _, err := Decrypt(bytes.Repeat([]byte{8}, 32), ct); err == nil {
		t.Fatal("wrong key must fail")
	}
}
