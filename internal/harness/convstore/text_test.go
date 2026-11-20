package convstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   \n\t  ", ""},
		{"New Session", ""},
		{"New session - 2026-01-01", ""},
		{"Untitled", ""},
		{"<system-reminder>ignore me</system-reminder>", ""},
		{"<session_context>header <loaded_context><hook_context>inner</hook_context></loaded_context></session_context>", ""},
		{"<t3_code_instructions>rules here</t3_code_instructions>\n\nFix streaming parser", "Fix streaming parser"},
		{"<user_request> hi </user_request> <runtime_info>meta</runtime_info>", "hi"},
		{"<USER_REQUEST>\n  Fix   auth\tbug\n</USER_REQUEST>", "Fix auth bug"},
		{"<user_input>Build SQLite store</user_input>", "Build SQLite store"},
		{"/compact", ""},
		{"/model claude-opus", ""},
		{"/plan implement search engine", "implement search engine"},
	}
	for _, tc := range cases {
		if got := CleanTitle(tc.in); got != tc.want {
			t.Errorf("CleanTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanMessageText(t *testing.T) {
	in := "<USER_REQUEST>Hello\n\n\n\n<system-reminder>secret</system-reminder>\nimage: data:image/png;base64," + strings.Repeat("A", 80) + "\nworld</USER_REQUEST>"
	got, trunc := CleanMessageText(in)
	if trunc {
		t.Fatalf("expected non-truncated")
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "AAAA") {
		t.Fatalf("expected synthetic block and base64 stripped, got %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "[data]") || !strings.Contains(got, "world") {
		t.Fatalf("unexpected cleaned message: %q", got)
	}

	// Verify 16 KB cap with multi-byte UTF-8 boundary safety.
	huge := strings.Repeat("日", MaxMessageTextBytes)
	capped, isTrunc := CleanMessageText(huge)
	if !isTrunc {
		t.Fatalf("expected truncation for oversized message")
	}
	if len(capped) > MaxMessageTextBytes {
		t.Fatalf("capped len %d exceeds MaxMessageTextBytes %d", len(capped), MaxMessageTextBytes)
	}
}

func TestExtractIdentifiers(t *testing.T) {
	got := ExtractIdentifiers("Refactor handleAuthRefresh and sync_metadata_v2 in internal/harness/convstore/search.go")
	for _, want := range []string{"handle", "auth", "refresh", "sync", "metadata", "v2", "internal", "harness", "convstore", "search", "go"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("ExtractIdentifiers missing %q in %q", want, got)
		}
	}
}

func TestParseTimestampMs(t *testing.T) {
	ref := time.Date(2026, 5, 10, 12, 30, 0, 0, time.UTC)
	wantMs := ref.UnixMilli()

	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"rfc3339", ref.Format(time.RFC3339), wantMs},
		{"rfc3339nano", ref.Format(time.RFC3339Nano), wantMs},
		{"unix_sec", ref.Unix(), wantMs},
		{"unix_ms", wantMs, wantMs},
		{"unix_us", wantMs * 1000, wantMs},
		{"unix_ns", ref.UnixNano(), wantMs},
		{"float_ms", float64(wantMs), wantMs},
		{"json_number", json.Number("1700000000"), 1700000000000},
		{"string_sec", "1700000000", 1700000000000},
		{"invalid", "not-a-date", 0},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		if got := ParseTimestampMs(tc.in); got != tc.want {
			t.Errorf("ParseTimestampMs(%s=%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}
