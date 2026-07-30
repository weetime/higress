package main

import (
	"encoding/json"
	"testing"
)

func TestBillingKindOf(t *testing.T) {
	cases := map[string]string{
		"/v1/audio/speech":                   "speech",
		"/api/x/v1/audio/transcriptions?a=1": "transcription",
		"/v1/images/generations":             "images",
		"/v1/chat/completions":               "",
	}
	for p, want := range cases {
		if got := billingKindOf(p); got != want {
			t.Fatalf("kind(%s)=%q want %q", p, got, want)
		}
	}
}

func TestSpeechDims(t *testing.T) {
	if got := speechDims([]byte(`{"input":"你好世界abc"}`)); got != `{"chars":7}` { // rune count
		t.Fatalf("got %s", got)
	}
	if got := speechDims([]byte(`{}`)); got != "" {
		t.Fatalf("empty input must yield no dims, got %s", got)
	}
}

func TestImageDims(t *testing.T) {
	if got := imageDims([]byte(`{"quality":"hd","size":"1024x1792","n":2}`)); got != `{"quality":"hd","size":"1024x1792","n":2}` {
		t.Fatalf("got %s", got)
	}
	if got := imageDims([]byte(`{"size":"1024x1024"}`)); got != `{"size":"1024x1024","n":1}` { // n 缺省 1
		t.Fatalf("got %s", got)
	}
	if got := imageDims([]byte(`{"n":2}`)); got != "" { // size 必要维度
		t.Fatalf("missing size must yield no dims, got %s", got)
	}
	if got := imageDims([]byte(`{"size":"evil,x=1|2","n":2}`)); got != "" { // 白名单
		t.Fatalf("bad size must be dropped, got %s", got)
	}
	if got := imageDims([]byte(`{"quality":"HD!!","size":"1024x1024"}`)); got != `{"size":"1024x1024","n":1}` { // 非法 quality 单独丢
		t.Fatalf("got %s", got)
	}
}

func TestTranscriptionSeconds(t *testing.T) {
	if got := transcriptionDims([]byte(`{"text":"一 二 三。","usage":{"type":"duration","seconds":3.2}}`)); got != `{"duration_seconds":4}` { // ceil
		t.Fatalf("got %s", got)
	}
	if got := transcriptionDims([]byte(`{"text":"x","usage":{"prompt_tokens":5}}`)); got != "" { // 非 duration 不猜
		t.Fatalf("got %s", got)
	}
}

// TestEscapeJSONString covers plain/quotes/backslash/mixed inputs (Task 16
// e2e fix round 2, console-ui#1666): billing_dims must be written to filter
// state in JSON-string-escaped form, since the access log format string
// embeds it via a byte-level splice, not a JSON-aware one.
func TestEscapeJSONString(t *testing.T) {
	cases := map[string]string{
		`hello`:         `hello`,
		`say "hi"`:      `say \"hi\"`,
		`back\slash`:    `back\\slash`,
		`mix "a" \ b`:   `mix \"a\" \\ b`,
		`{"chars":156}`: `{\"chars\":156}`,
	}
	for in, want := range cases {
		if got := escapeJSONString(in); got != want {
			t.Fatalf("escapeJSONString(%q)=%q want %q", in, got, want)
		}
	}
}

// TestBillingDimsRoundTripThroughAccessLogEmbedding simulates the real
// pipeline: writeBillingDims stores the escaped form; Envoy's access log
// formatter splices it, byte-for-byte, into a JSON template; fluent-bit then
// parses that log line as JSON. Assert the spliced line is valid JSON and
// that fluent-bit's outer parse round-trips the field back to the original
// flat dims string, so Task 12's Lua regex sees the unescaped flat JSON.
func TestBillingDimsRoundTripThroughAccessLogEmbedding(t *testing.T) {
	dims := `{"chars":156}`

	stored := escapeJSONString(dims)
	if stored != `{\"chars\":156}` {
		t.Fatalf("stored=%q", stored)
	}

	// Byte-level splice, exactly what %FILTER_STATE(wasm.billing_dims:PLAIN)% does.
	logLine := `{"billing_dims":"` + stored + `"}`

	if !json.Valid([]byte(logLine)) {
		t.Fatalf("spliced log line is not valid JSON: %s", logLine)
	}

	var parsed struct {
		BillingDims string `json:"billing_dims"`
	}
	if err := json.Unmarshal([]byte(logLine), &parsed); err != nil {
		t.Fatalf("failed to unmarshal spliced log line: %v", err)
	}
	if parsed.BillingDims != dims {
		t.Fatalf("round-tripped billing_dims=%q want %q", parsed.BillingDims, dims)
	}
}
