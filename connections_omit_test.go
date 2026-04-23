package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOmitPath covers the path patterns the integration catalog is
// expected to use. Each case starts from a realistic-ish upstream
// JSON shape (modelled on Deepgram's /v1/listen response) and asserts
// the specified paths are gone + untouched paths are intact.
func TestOmitPath(t *testing.T) {
	shape := `{
		"metadata": {
			"duration": 1085.22,
			"sha256": "be1d...",
			"model_info": {"arch":"nova-3"}
		},
		"results": {
			"channels": [
				{
					"alternatives": [
						{
							"transcript": "hello world",
							"confidence": 0.99,
							"words": [
								{"word":"hello","start":0.0,"end":0.5,"confidence":0.99},
								{"word":"world","start":0.5,"end":1.0,"confidence":0.99}
							]
						}
					]
				}
			],
			"utterances": [{"start":0,"end":1,"transcript":"hello world"}]
		}
	}`
	var data any
	if err := json.Unmarshal([]byte(shape), &data); err != nil {
		t.Fatalf("setup: %v", err)
	}
	paths := []string{
		"metadata.sha256",
		"metadata.model_info",
		"results.channels[].alternatives[].words",
		"results.utterances",
	}
	for _, p := range paths {
		data = omitPath(data, p)
	}
	out, _ := json.Marshal(data)
	s := string(out)

	for _, shouldBeGone := range []string{"sha256", "model_info", `"words"`, "utterances"} {
		if strings.Contains(s, shouldBeGone) {
			t.Errorf("expected %q to be stripped; still present in: %s", shouldBeGone, s)
		}
	}
	for _, shouldStay := range []string{"transcript", "hello world", "confidence", "1085.22"} {
		if !strings.Contains(s, shouldStay) {
			t.Errorf("expected %q to remain; missing from: %s", shouldStay, s)
		}
	}
}

// Silent on schema drift: a non-matching path is a no-op, not an error.
func TestOmitPath_MissingPathIsNoop(t *testing.T) {
	var data any
	json.Unmarshal([]byte(`{"foo":1}`), &data)
	omitPath(data, "nothing.here[].at_all")
	omitPath(data, "foo.nested.deeper")
	out, _ := json.Marshal(data)
	if !strings.Contains(string(out), `"foo":1`) {
		t.Errorf("expected foo untouched, got %s", string(out))
	}
}
