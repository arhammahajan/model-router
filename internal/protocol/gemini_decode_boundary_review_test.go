package protocol

import "testing"

func TestGeminiDroppedControlBoundaryTypes(t *testing.T) {
	for _, raw := range []string{
		`{"contents":[{"parts":[{"text":"hi"}]}],"toolConfig":{"retrievalConfig":{"latLng":{"latitude":"0.5"}}}}`,
		`{"contents":[{"parts":[{"text":"hi","videoMetadata":{"fps":"1"}}]}]}`,
		`{"contents":[{"parts":[{"text":"hi","mediaResolution":{"value":{"level":"MEDIA_RESOLUTION_LOW"}}}]}]}`,
	} {
		expectGemini400(t, raw, false)
	}
}
