// Gemini response candidate-array validation shared by downstream dialects.
package protocol

import "encoding/json"

// geminiNormalizeCandidates is the policy-neutral candidate-array shape
// helper shared by the Gemini response bridges. It reports the candidacy
// array as absent when the member is missing, null, or an empty array; a
// malformed (non-array) member reports malformed=true; otherwise the
// decoded candidates are returned. Callers apply their own destination
// policy (blocked contradiction vs multiple-choices vs invalid) so one
// shape check serves JSON and both stream framings coherently.
func geminiNormalizeCandidates(raw json.RawMessage, present bool) (cands []json.RawMessage, absent bool, malformed bool) {
	if !present || isNull(raw) {
		return nil, true, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false, true
	}
	if len(arr) == 0 {
		return nil, true, false
	}
	return arr, false, false
}
