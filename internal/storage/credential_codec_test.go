package storage

import (
	"testing"
)

func TestCredentialCodecWritesAndReadsV3(t *testing.T) {
	codec, err := newCredentialCodec("test-master-key")
	if err != nil {
		t.Fatalf("newCredentialCodec returned error: %v", err)
	}
	encoded, err := codec.encryptValue("secret-value")
	if err != nil {
		t.Fatalf("encryptValue returned error: %v", err)
	}
	if !isEncryptedValue(encoded) || len(encoded) <= len(encryptedValuePrefixV3) || encoded[:len(encryptedValuePrefixV3)] != encryptedValuePrefixV3 {
		t.Fatalf("encoded value = %q, want v3 encrypted value", encoded)
	}
	decoded, err := codec.decryptValue(encoded)
	if err != nil {
		t.Fatalf("decryptValue(v3) returned error: %v", err)
	}
	if decoded != "secret-value" {
		t.Fatalf("decoded v3 = %q, want secret-value", decoded)
	}
}
