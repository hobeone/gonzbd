package main

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		line      string
		wantKind  commandKind
		wantMsgID string
	}{
		{"CAPABILITIES\r\n", cmdCapabilities, ""},
		{"capabilities\r\n", cmdCapabilities, ""},
		{"AUTHINFO USER joe\r\n", cmdAuthInfo, ""},
		{"AUTHINFO PASS secret\r\n", cmdAuthInfo, ""},
		{"BODY <abc123@example>\r\n", cmdBody, "abc123@example"},
		{"body <abc123@example>\r\n", cmdBody, "abc123@example"},
		{"STAT <def456@example>\r\n", cmdStat, "def456@example"},
		{"QUIT\r\n", cmdQuit, ""},
		{"XOVER 1-100\r\n", cmdOther, ""},
	}
	for _, tt := range tests {
		kind, msgID := parseCommand(tt.line)
		if kind != tt.wantKind {
			t.Errorf("parseCommand(%q) kind = %v, want %v", tt.line, kind, tt.wantKind)
		}
		if msgID != tt.wantMsgID {
			t.Errorf("parseCommand(%q) messageID = %q, want %q", tt.line, msgID, tt.wantMsgID)
		}
	}
}

func TestExtractMessageID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"<abc@host>", "abc@host"},
		{"abc@host", "abc@host"},
		{"  <abc@host>  ", "abc@host"},
	}
	for _, tt := range tests {
		if got := extractMessageID(tt.in); got != tt.want {
			t.Errorf("extractMessageID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsMultilineResponse(t *testing.T) {
	tests := []struct {
		kind commandKind
		code int
		want bool
	}{
		{cmdCapabilities, 101, true},
		{cmdBody, 222, true},
		{cmdBody, 430, false},
		{cmdStat, 223, false},
		{cmdStat, 430, false},
		{cmdAuthInfo, 281, false},
		{cmdQuit, 205, false},
		{cmdOther, 200, false},
	}
	for _, tt := range tests {
		if got := isMultilineResponse(tt.kind, tt.code); got != tt.want {
			t.Errorf("isMultilineResponse(%v, %d) = %v, want %v", tt.kind, tt.code, got, tt.want)
		}
	}
}

func TestStatusCode(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"222 0 <id> body follows\r\n", 222},
		{"430 no such article\r\n", 430},
		{"\r\n", 0},
		{"ab\r\n", 0},
		{"abc\r\n", 0},
	}
	for _, tt := range tests {
		if got := statusCode(tt.line); got != tt.want {
			t.Errorf("statusCode(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}
