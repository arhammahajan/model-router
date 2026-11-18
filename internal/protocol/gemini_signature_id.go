// Gemini thought-signature identity wrapping.
//
// Gemini 4 function calls carry an opaque part-level thoughtSignature
// that the API requires back on the functionCall parts of subsequent
// turns (missing signatures are a hard 400 on function-calling
// histories). The router is stateless: no conversation store may hold
// the signature between requests, and no harness dialect defines a
// field for provider-private tool-call state. The one channel every
// harness echoes verbatim is the tool-call identity, so lossy Gemini
// bridges mint a wrapped public identity carrying the signature:
//
//	gts1_<base64url(JSON {"s":<sig>,"i":<innerID>})>
//
// The wrapped form is produced only at Gemini source decode (complete
// and stream rows) when the provider policy is lossy; request decoders
// of every ingress dialect unwrap the canonical form before identity
// matching, restoring the provider identity for the upstream wire and
// attaching the signature to the IR ToolCall. Ownership is decided by
// strict canonical decode, never by the prefix alone: a malformed,
// oversized or non-canonical candidate is an ordinary opaque id and
// passes through untouched, exactly like the gemini-compat-call-
// namespace policy (a provider may legitimately supply lookalikes).
package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// geminiSignatureIDPrefix marks wrapped thought-signature identities.
const geminiSignatureIDPrefix = "gts1_"

// geminiSignatureMaxBytes bounds the signature payload carried inside a
// wrapped identity. Observed signatures are a few hundred bytes; the
// bound keeps wrapped identities small and refuse-to-wrap behavior
// explicit for pathological upstreams.
const geminiSignatureMaxBytes = 64 << 10

type geminiSignatureID struct {
	S string `json:"s"`
	I string `json:"i"`
}

// wrapGeminiSignatureID mints the public identity carrying sig and the
// provider (or router-minted) inner identity. Empty sig, oversized sig
// or empty inner means no wrap: the caller keeps the inner identity
// unchanged (a text-part signature has no identity to ride on and is
// simply dropped as before).
func wrapGeminiSignatureID(sig, innerID string) string {
	if sig == "" || innerID == "" || len(sig) > geminiSignatureMaxBytes {
		return innerID
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(mustMarshalSignatureID(sig, innerID)))
	return geminiSignatureIDPrefix + enc
}

// unwrapGeminiSignatureID reports whether id is a canonical wrapped
// identity, restoring the signature and the inner provider identity.
// Only the exact canonical form qualifies: the prefix plus base64url
// of a two-member JSON object with nonempty string values. Anything
// else - malformed base64, extra or missing members, empty values -
// is not ours and the caller treats the value as a plain opaque id.
func unwrapGeminiSignatureID(id string) (sig, innerID string, ok bool) {
	if len(id) <= len(geminiSignatureIDPrefix) || id[:len(geminiSignatureIDPrefix)] != geminiSignatureIDPrefix {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id[len(geminiSignatureIDPrefix):])
	if err != nil || len(raw) == 0 || len(raw) > geminiSignatureMaxBytes {
		return "", "", false
	}
	var v geminiSignatureID
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", false
	}
	if v.S == "" || v.I == "" || len(v.S) > geminiSignatureMaxBytes {
		return "", "", false
	}
	return v.S, v.I, true
}

func mustMarshalSignatureID(sig, innerID string) []byte {
	b, err := json.Marshal(geminiSignatureID{S: sig, I: innerID})
	if err != nil {
		// Both members are plain strings; marshaling cannot fail.
		panic(fmt.Sprintf("protocol: signature id marshal: %v", err))
	}
	return b
}
