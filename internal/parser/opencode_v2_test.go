package parser

import (
	"strings"
	"testing"
)

func TestParseOpenCodeV2(t *testing.T) {
	raw := loadFixture(t, "opencode-v2.jsonl")
	s, err := Parse(AgentOpenCode, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages=%d", len(s.Messages))
	}
	if s.Messages[0].Content != "hello" || s.Messages[1].Content != "answer" || s.Messages[1].ThinkingText != "think" || s.Messages[1].Model != "openai/gpt-6-sol" {
		t.Fatalf("messages=%+v", s.Messages)
	}
	if len(s.ToolCalls) != 1 || s.ToolCalls[0].ResultStatus != "ok" || !strings.Contains(s.ToolCalls[0].ResultBody, "file:///output") {
		t.Fatalf("tools=%+v", s.ToolCalls)
	}
	if len(s.Attachments) != 1 || s.Attachments[0].Content != "hello" || s.Attachments[0].Filename != "hello.txt" {
		t.Fatalf("attachments=%+v", s.Attachments)
	}
	if len(s.UsageEvent) != 1 || s.UsageEvent[0].Input != 10 || s.UsageEvent[0].ReportedCostUSD == nil || *s.UsageEvent[0].ReportedCostUSD != 0.012 {
		t.Fatalf("usage=%+v", s.UsageEvent)
	}
	if s.Identity.ParentSourceID != "ses_parent" || s.Identity.SubagentName != "explore" || s.EndedAt.UnixMilli() != 1400 {
		t.Fatalf("identity/time=%+v %s", s.Identity, s.EndedAt)
	}
	if len(s.Events) < 1 || !strings.Contains(s.Events[0].AttrsJSON, "failed") {
		t.Fatalf("events=%+v", s.Events)
	}
}

func TestParseOpenCodeV2Lifecycle(t *testing.T) {
	raw := []byte(`{"type":"session","id":"s","directory":"/old","time_created":1}
{"type":"message_v2","id":"1","data":{"type":"model-switched","model":{"providerID":"p","id":"m"},"time":{"created":2}}}
{"type":"message_v2","id":"2","data":{"type":"agent-switched","agent":"explore","time":{"created":3}}}
{"type":"message_v2","id":"3","data":{"type":"location-switched","location":{"directory":"/new"},"time":{"created":4}}}
{"type":"message_v2","id":"4","data":{"type":"synthetic","text":"injected context","time":{"created":5}}}
{"type":"message_v2","id":"5","data":{"type":"system","text":"world update","time":{"created":6}}}
{"type":"message_v2","id":"6","data":{"type":"skill","skill":"test","name":"test","text":"skill body","time":{"created":7}}}
{"type":"message_v2","id":"7","data":{"type":"compaction","status":"completed","reason":"auto","summary":"summary","recent":"recent","model":{"id":"m","providerID":"p"},"tokens":{"input":1,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0,"time":{"created":8}}}
{"type":"message_v2","id":"8","data":{"type":"shell","shellID":"sh_test","command":"false","status":"exited","exit":1,"output":{"output":"failed","cursor":6,"size":6,"truncated":false},"time":{"created":9,"completed":10}}}
{"type":"message_v2","id":"9","data":{"type":"idle","outcome":"interrupted","time":{"created":11}}}
`)
	s, err := Parse(AgentOpenCode, raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cwd != "/new" || s.EndedAt.UnixMilli() != 11 {
		t.Fatalf("session=%+v", s)
	}
	if len(s.Messages) != 5 || s.Messages[0].Role != RoleContext || s.Messages[2].Content != "skill body" || s.Messages[3].Content != "summary\nrecent" {
		t.Fatalf("messages=%+v", s.Messages)
	}
	if len(s.ToolCalls) != 1 || s.ToolCalls[0].ResultStatus != "error" || s.ToolCalls[0].ResultBody != "failed" {
		t.Fatalf("tools=%+v", s.ToolCalls)
	}
	if len(s.UsageEvent) != 1 || s.UsageEvent[0].ReportedCostUSD == nil || *s.UsageEvent[0].ReportedCostUSD != 0 {
		t.Fatalf("usage=%+v", s.UsageEvent)
	}
	if len(s.Events) != 5 || s.Events[4].Kind != EventTurnAborted {
		t.Fatalf("events=%+v", s.Events)
	}
	if got := s.Events[0].AttrsJSON; got != `{"model":"m","provider":"p"}` {
		t.Fatalf("model switch attrs=%s", got)
	}
}

func TestParseOpenCodeV2RejectsUnsupportedContent(t *testing.T) {
	raw := []byte(`{"type":"message_v2","id":"a","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"future"}],"time":{"created":1}}}`)
	_, err := Parse(AgentOpenCode, raw)
	if err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseOpenCodeV2EncryptedReasoning(t *testing.T) {
	for _, state := range []string{`{"reasoningEncryptedContent":"ABCD"}`, `{"signature":"ABCD"}`, `{"redactedData":"ABCD"}`, `{"xai":{"reasoningEncryptedContent":"ABCD"}}`} {
		raw := []byte(`{"type":"message_v2","id":"a","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"reasoning","text":"","state":` + state + `}],"time":{"created":1,"completed":2}}}`)
		s, err := Parse(AgentOpenCode, raw)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Messages[0].HasThinking || s.Messages[0].ThinkingBytes != 4 {
			t.Fatalf("state %s: message=%+v", state, s.Messages[0])
		}
		if s.EndedAt.UnixMilli() != 2 {
			t.Fatalf("completion timestamp=%s", s.EndedAt)
		}
	}
}

func TestParseOpenCodeV2RejectsUnknownMessageType(t *testing.T) {
	_, err := Parse(AgentOpenCode, []byte(`{"type":"message_v2","id":"future","data":{"type":"future","time":{"created":1}}}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported message type") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseOpenCodeV2RejectsMalformedBodyReference(t *testing.T) {
	for _, ref := range []string{
		`{"__akari_cas__":1}`, `{"__akari_cas__":2,"sha256":"` + strings.Repeat("a", 64) + `","bytes":1,"media_type":"application/json"}`,
		`{"__akari_cas__":1,"sha256":"invalid","bytes":1,"media_type":"application/json"}`,
		`{"__akari_cas__":1,"sha256":"` + strings.Repeat("a", 64) + `","bytes":-1,"media_type":"application/json"}`,
		`{"__akari_cas__":1,"sha256":"` + strings.Repeat("a", 64) + `","bytes":1.5,"media_type":"application/json"}`,
		`{"__akari_cas__":1,"sha256":"` + strings.Repeat("a", 64) + `","bytes":1,"media_type":""}`,
	} {
		t.Run(ref, func(t *testing.T) {
			raw := []byte(`{"type":"message_v2","id":"bad","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"tool","id":"call","name":"read","state":{"status":"completed","input":` + ref + `,"content":[{"type":"text","text":"done"}]}}],"time":{"created":1}}}`)
			_, err := Parse(AgentOpenCode, raw)
			if err == nil || !strings.Contains(err.Error(), "reference") {
				t.Fatalf("malformed reference accepted: %v", err)
			}
		})
	}
}

func TestParseOpenCodeV2BodyReferenceLookalikeRemainsInput(t *testing.T) {
	raw := []byte(`{"type":"message_v2","id":"ordinary","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"tool","id":"call","name":"read","state":{"status":"completed","input":{"sha256":"ordinary string","bytes":-1},"content":[{"type":"text","text":"done"}]}}],"time":{"created":1}}}`)
	parsed, err := Parse(AgentOpenCode, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.ToolCalls) != 1 || !strings.Contains(parsed.ToolCalls[0].InputJSON, "ordinary string") {
		t.Fatalf("input lost: %+v", parsed.ToolCalls)
	}
}

func TestParseOpenCodeV2RejectsUnboundedSubagentRole(t *testing.T) {
	raw := []byte(`{"type":"message_v2","id":"oversize","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"tool","id":"call","name":"subagent","state":{"status":"completed","input":{"agent":"` + strings.Repeat("a", maxOpenCodeV2SubagentType+1) + `"},"content":[{"type":"text","text":"done"}]}}],"time":{"created":1}}}`)
	_, err := Parse(AgentOpenCode, raw)
	if err == nil || !strings.Contains(err.Error(), "subagent role") {
		t.Fatalf("error=%v", err)
	}
}
