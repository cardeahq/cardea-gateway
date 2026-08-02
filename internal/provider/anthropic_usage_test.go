package provider

import "testing"

func TestParseUsage_NonStreaming(t *testing.T) {
	sent, received, ok := ParseUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`))
	if !ok {
		t.Fatal("ParseUsage: ok = false, want true")
	}
	if sent != 10 {
		t.Fatalf("sent = %d, want 10", sent)
	}
	if received != 5 {
		t.Fatalf("received = %d, want 5", received)
	}
}

func TestParseUsage_IncludesCachedTokensInSent(t *testing.T) {
	body := `{"usage":{"input_tokens":2,"cache_creation_input_tokens":1400,"cache_read_input_tokens":3200,"output_tokens":132}}`
	sent, received, ok := ParseUsage([]byte(body))
	if !ok {
		t.Fatal("ParseUsage: ok = false, want true")
	}
	if sent != 4602 {
		t.Fatalf("sent = %d, want 4602 (input + cache_creation + cache_read)", sent)
	}
	if received != 132 {
		t.Fatalf("received = %d, want 132", received)
	}
}

func TestParseUsage_ParsesStreamedUsage(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta"}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	sent, received, ok := ParseUsage([]byte(body))
	if !ok {
		t.Fatal("ParseUsage: ok = false, want true")
	}
	if sent != 25 {
		t.Fatalf("sent = %d, want 25", sent)
	}
	if received != 15 {
		t.Fatalf("received = %d, want 15", received)
	}
}

func TestParseUsage_IncludesCachedTokensInStreamedSent(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2,"cache_creation_input_tokens":1400,"cache_read_input_tokens":3200,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":132}}` + "\n\n"

	sent, received, ok := ParseUsage([]byte(body))
	if !ok {
		t.Fatal("ParseUsage: ok = false, want true")
	}
	if sent != 4602 {
		t.Fatalf("sent = %d, want 4602 (input + cache_creation + cache_read)", sent)
	}
	if received != 132 {
		t.Fatalf("received = %d, want 132", received)
	}
}

func TestParseUsage_IgnoresNonUsageBodies(t *testing.T) {
	_, _, ok := ParseUsage([]byte("data: {\"type\":\"content_block_delta\"}\n\n"))
	if ok {
		t.Fatal("ParseUsage: ok = true, want false for a body with no usage info")
	}
}
