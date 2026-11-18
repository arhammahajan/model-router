package discovery

import "testing"

func TestCacheSecretCorpusPreservesPythonEncodedForms(t *testing.T) {
	for _, tc := range []struct{ key, encoded string }{
		{"a b+c", "a%20b%2Bc"},
		{"a\"<", `a\"<`},
		{"k-😀-key", `k-\ud83d\ude00-key`},
	} {
		cfg := cacheTestConfig(t, t.TempDir())
		cfg.Providers["up"].APIKey = tc.key
		if newCacheSanitizer(cfg).safeID(tc.encoded) {
			t.Fatal("encoded configured secret passed cache admission")
		}
	}
}

func TestCacheJSONIdentityEncodingNeverRewrites(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"id":"m\ud83d\ude00"}`, true},
		{`{"id":"literal\\ud800"}`, true},
		{`{"id":"\ud800"}`, false},
		{`{"id":"\udc00"}`, false},
		{`{"id":"\ud800\u0041"}`, false},
	} {
		_, err := decodeJSONObject([]byte(tc.raw))
		if (err == nil) != tc.want {
			t.Fatalf("encoding admission for %q: err=%v", tc.raw, err)
		}
	}
}

func TestCacheURLUserinfoIsDecodedExactlyOnce(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	cfg.Catalog.URL = "http://user%252ffoo:p%252bword@example.test/catalog?q=query%252fvalue"
	san := newCacheSanitizer(cfg)
	for _, secret := range []string{"user%2ffoo", "p%2bword", "query%2fvalue"} {
		if san.safeID(secret) {
			t.Fatal("decoded URL credential passed cache admission")
		}
	}
}
