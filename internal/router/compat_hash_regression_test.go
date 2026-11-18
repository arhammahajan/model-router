package router

import (
	"encoding/json"
	"strings"
	"testing"
)

// Goldens from Python json.dumps with default ensure_ascii followed by SHA256.
func TestCompatHashExpandedChunks(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{strings.Repeat("é😀<>&\x00\b\t\n\f\r\x7f/\"\\", 37), "agy-acp-3825092bb540fd9a36341d4c0e2b4f5d5c657a8d98b3bd9e6242e53dd3eb0d22"},
		{strings.Repeat("a", 63), "agy-acp-e7d55ca10614618e5755859f93ac280712a04f76466e7bc65e4f488f1153ec70"},
		{strings.Repeat("é", 4097), "agy-acp-ac177a28d4b935688da3a928679dfcaa9424df142bb4721595da2d811178f3ea"},
	} {
		raw, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		got, empty, rerr := canonicalSessionHash(raw)
		if rerr != nil || empty || got != tc.want {
			t.Fatalf("hash mismatch: got %s empty=%v err=%v", got, empty, rerr)
		}
	}
}
