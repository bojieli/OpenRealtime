package authority

import (
	"bytes"
	"testing"
)

func TestSealedEffectReceiptCloseOverwritesRetainedKeyMaterial(t *testing.T) {
	provider, err := NewSealedEffectReceipts(EffectReceiptOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{0x7b}, 128)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(provider.key[:], make([]byte, len(provider.key))) {
		t.Fatal("provider started without key material")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(provider.key[:], make([]byte, len(provider.key))) ||
		!bytes.Equal(provider.noncePrefix[:], make([]byte, len(provider.noncePrefix))) ||
		provider.nonceCounter.Load() != 0 || provider.clock != nil || !provider.closed {
		t.Fatalf("provider close retained key lifecycle state: key=%x nonce=%x/%d clock=%v closed=%v",
			provider.key, provider.noncePrefix, provider.nonceCounter.Load(), provider.clock != nil, provider.closed)
	}
}
