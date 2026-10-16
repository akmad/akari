package parser

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenCodeV2ExtractionNativeBodies(t *testing.T) {
	cases := []struct {
		name, line      string
		contents, media []string
	}{
		{"mixed tool output", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","id":"call_1","name":"read","state":{"status":"completed","input":{"filePath":"src/a.go"},"content":[{"type":"text","text":"read it"},{"type":"file","uri":"file:///tmp/a.png","mime":"image/png","name":"a.png"}]}}]}}`, []string{`{"filePath":"src/a.go"}`, `[{"type":"text","text":"read it"},{"type":"file","uri":"file:///tmp/a.png","mime":"image/png","name":"a.png"}]`}, []string{"application/json", "application/json"}},
		{"input after output", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"content":[{"type":"text","text":"done"}],"input":{"command":"go test"},"status":"completed"}}]}}`, []string{`[{"type":"text","text":"done"}]`, `{"command":"go test"}`}, []string{"application/json", "application/json"}},
		{"structured tool error", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"error","input":{},"error":{"type":"tool.failed","message":"permission denied"}}}]}}`, []string{`{}`, `{"type":"tool.failed","message":"permission denied"}`}, []string{"application/json", "application/json"}},
		{"error with content", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"error","input":{},"content":[{"type":"text","text":"stderr"}],"error":{"type":"tool.failed","message":"failed"}}}]}}`, []string{`{}`, `[{"type":"text","text":"stderr"}]`}, []string{"application/json", "application/json"}},
		{"streaming input", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"streaming","input":"{\"command\":"}}]}}`, []string{`"{\"command\":"`}, []string{"application/json"}},
		{"running input", `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"running","input":{"command":"sleep 1"}}}]}}`, []string{`{"command":"sleep 1"}`}, []string{"application/json"}},
		{"user files with arbitrary MIME", `{"type":"message_v2","data":{"type":"user","text":"attached","files":[{"data":"aGVsbG8=","mime":"text/plain","name":"notes.txt","source":{"type":"inline"}},{"mime":"application/pdf","data":"AAEC/w==","name":"a.pdf","source":{"type":"uri","uri":"file:///tmp/a.pdf"}}]}}`, []string{"hello", "\x00\x01\x02\xff"}, []string{"text/plain", "application/pdf"}},
		{"shell output", `{"type":"message_v2","data":{"type":"shell","command":"go test","status":"exited","output":{"output":"ok\n","cursor":3,"size":3,"truncated":false}}}`, []string{"ok\n"}, []string{"text/plain"}},
		{"other event", `{"type":"message_v2","data":{"type":"model-switched","model":{"id":"a"}}}`, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := opencodeV2BodyFields(gjson.Parse(tc.line))
			if len(fields) != len(tc.contents) {
				t.Fatalf("fields = %d, want %d", len(fields), len(tc.contents))
			}
			for i, f := range fields {
				if f.content != tc.contents[i] || f.media != tc.media[i] {
					t.Errorf("body %d = %q (%s), want %q (%s)", i, f.content, f.media, tc.contents[i], tc.media[i])
				}
				if i > 0 && f.start < fields[i-1].end {
					t.Errorf("body %d is out of source order", i)
				}
			}
			// Nonzero line offset catches accidental sub-object-relative spans.
			reader := strings.NewReader("prefix" + tc.line)
			s := &lineSource{ctx: context.Background(), f: reader, base: 6, size: int64(len(tc.line))}
			var located []BodyLocation
			if err := locateOpenCodeV2(s, func(loc BodyLocation) error { located = append(located, loc); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(located) != len(fields) {
				t.Fatalf("streaming bodies = %d, buffered = %d", len(located), len(fields))
			}
			for i, loc := range located {
				content, err := io.ReadAll(CanonicalBodyReader(s.ctx, reader, s.base, loc.Span, loc.Kind))
				if err != nil {
					t.Fatal(err)
				}
				f := fields[i]
				if string(content) != f.content || loc.Media != f.media || loc.Span.Start != int64(f.start) || loc.Span.End != int64(f.end) {
					t.Errorf("body %d streaming differs from buffered: %q, %+v vs %+v", i, content, loc, f)
				}
				if loc.FilePath != sentinelFilePath(f.kind, f.media, f.content) || loc.Detail != sentinelDetail(f.kind, f.media, f.content) {
					t.Errorf("body %d input projections differ", i)
				}
			}
		})
	}
}

func TestOpenCodeV2ExtractionPreservesMalformedAndExistingReferences(t *testing.T) {
	ref := string(SentinelBytes(HashString("body"), 4, "application/json", "", "", false))
	line := `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"completed","input":` + ref + `,"content":` + ref + `}}]}}`
	if fields := opencodeV2BodyFields(gjson.Parse(line)); len(fields) != 0 {
		t.Fatalf("existing references lifted again: %+v", fields)
	}
	s := &lineSource{ctx: context.Background(), f: strings.NewReader(line), size: int64(len(line))}
	if err := locateOpenCodeV2(s, func(BodyLocation) error { t.Error("existing reference emitted again"); return nil }); err != nil {
		t.Fatal(err)
	}
	bad := `{"type":"message_v2","data":{"type":"user","files":[{"mime":"application/pdf","data":"invalid base64!"}]}}`
	if fields := opencodeV2BodyFields(gjson.Parse(bad)); len(fields) != 0 {
		t.Fatal("malformed attachment must stay inline")
	}
	s = &lineSource{ctx: context.Background(), f: strings.NewReader(bad), size: int64(len(bad))}
	var decodeErr error
	if err := locateOpenCodeV2(s, func(loc BodyLocation) error {
		_, decodeErr = io.Copy(io.Discard, CanonicalBodyReader(s.ctx, s.f, 0, loc.Span, loc.Kind))
		return decodeErr
	}); err == nil || decodeErr == nil {
		t.Fatal("streaming malformed base64 did not surface an error")
	}
}

func TestOpenCodeV2ExtractionPropagatesFailures(t *testing.T) {
	line := `{"type":"message_v2","data":{"type":"assistant","content":[{"type":"tool","state":{"status":"completed","input":{},"content":[{"type":"text","text":"ok"}]}}]}}`
	s := &lineSource{ctx: context.Background(), f: strings.NewReader(line), size: int64(len(line))}
	want := errors.New("stop upload")
	if err := locateOpenCodeV2(s, func(BodyLocation) error { return want }); !errors.Is(err, want) {
		t.Fatalf("emit error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	if err := locateOpenCodeV2(s, func(BodyLocation) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	s.ctx = context.Background()
	s.size += 10
	if err := locateOpenCodeV2(s, func(BodyLocation) error { return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read = %v", err)
	}
}

func TestOpenCodeV2ExtractionRoundTrip(t *testing.T) {
	raw := []byte(`{"type":"session","id":"ses_v2","directory":"/tmp/project","time_created":1}
{"type":"message_v2","id":"msg_user","time_created":2,"data":{"type":"user","time":{"created":2},"text":"read the attachments","files":[{"data":"aGVsbG8=","mime":"text/plain","name":"notes.txt","source":{"type":"inline"}},{"data":"AAEC/w==","mime":"application/pdf","name":"a.pdf","source":{"type":"inline"}}]}}
{"type":"message_v2","id":"msg_assistant","time_created":3,"data":{"type":"assistant","time":{"created":3,"completed":4},"agent":"build","model":{"id":"test","providerID":"test"},"content":[{"type":"text","text":"Results"},{"type":"tool","id":"call_read","name":"read","state":{"status":"completed","input":{"filePath":"src/a.go"},"content":[{"type":"text","text":"package a"},{"type":"file","uri":"file:///tmp/a.png","mime":"image/png","name":"a.png"}]}},{"type":"tool","id":"call_fail","name":"shell","state":{"status":"error","input":{"command":"false"},"error":{"type":"tool.failed","message":"exit 1"}}}]}}
`)
	want := inlineBodies(t, AgentOpenCode, raw)
	rewritten, bodies, err := ExtractBodies(AgentOpenCode, raw, idEncoder{})
	if err != nil {
		t.Fatal(err)
	}
	assertSameBodies(t, bodies, want)
	parsed, err := Parse(AgentOpenCode, rewritten)
	if err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	if len(parsed.Attachments) != 2 || len(parsed.ToolCalls) != 2 {
		t.Fatalf("round trip lost records: %+v", parsed)
	}
	for i, attachment := range parsed.Attachments {
		if attachment.SHA256 == "" || attachment.Content != "" {
			t.Errorf("attachment %d not referenced: %+v", i, attachment)
		}
	}
	for i, call := range parsed.ToolCalls {
		if call.InputSHA256 == "" || call.ResultSHA256 == "" || call.InputJSON != "" || call.ResultBody != "" {
			t.Errorf("tool %d not referenced: %+v", i, call)
		}
	}
	if parsed.ToolCalls[0].FilePath != "src/a.go" || parsed.ToolCalls[1].Detail != "false" || parsed.ToolCalls[1].ResultStatus != "error" {
		t.Fatalf("round trip lost projections: %+v", parsed.ToolCalls)
	}
	again, more, err := ExtractBodies(AgentOpenCode, rewritten, idEncoder{})
	if err != nil || len(more) != 0 || string(again) != string(rewritten) {
		t.Fatalf("rewrite not idempotent: %v, %d more bodies", err, len(more))
	}
}

func TestOpenCodeV2ExtractionShellRoundTrip(t *testing.T) {
	raw := []byte(`{"type":"message_v2","id":"msg_shell","data":{"type":"shell","time":{"created":1,"completed":2},"shellID":"sh_1","command":"go test","status":"exited","exit":1,"output":{"output":"failed\n","cursor":7,"size":10,"truncated":true}}}` + "\n")
	rewritten, bodies, err := ExtractBodies(AgentOpenCode, raw, idEncoder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || string(bodies[0].Stored) != "failed\n" || bodies[0].MediaType != "text/plain" {
		t.Fatalf("shell output = %+v", bodies)
	}
	output := gjson.GetBytes(rewritten, "data.output")
	if output.Get("cursor").Int() != 7 || output.Get("size").Int() != 10 || !output.Get("truncated").Bool() {
		t.Fatalf("shell metadata lost: %s", rewritten)
	}
	parsed, err := Parse(AgentOpenCode, rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.ToolCalls) != 1 || parsed.ToolCalls[0].ResultSHA256 != bodies[0].SHA256 || parsed.ToolCalls[0].ResultStatus != "error" {
		t.Fatalf("shell result lost: %+v", parsed.ToolCalls)
	}
}

func TestOpenCodeV2ExtractionPreservesSubagentType(t *testing.T) {
	for _, shape := range []struct{ name, input string }{{"subagent", `{"agent":"explore","description":"Map auth","prompt":"long prompt"}`}, {"task", `{"subagent_type":"explore","description":"Map auth","prompt":"long prompt"}`}} {
		t.Run(shape.name, func(t *testing.T) {
			raw := []byte(`{"type":"message_v2","id":"a","data":{"type":"assistant","agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"tool","id":"call","name":"` + shape.name + `","state":{"status":"completed","input":` + shape.input + `,"content":[{"type":"text","text":"done"}],"metadata":{"sessionID":"child"}},"time":{"created":2}}],"time":{"created":1,"completed":3}}}` + "\n")
			inline, err := Parse(AgentOpenCode, raw)
			if err != nil {
				t.Fatal(err)
			}
			lifted, _, err := ExtractBodies(AgentOpenCode, raw, idEncoder{})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(AgentOpenCode, lifted)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Events) != 1 || parsed.Events[0].AttrsJSON != inline.Events[0].AttrsJSON {
				t.Fatalf("lost subagent event: inline=%+v lifted=%+v", inline.Events, parsed.Events)
			}
			if gjson.Get(parsed.Events[0].AttrsJSON, "subagent_type").String() != "explore" {
				t.Fatalf("role missing: %+v", parsed.Events)
			}
		})
	}
}
