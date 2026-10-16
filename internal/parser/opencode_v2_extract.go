package parser

import (
	"sort"

	"github.com/tidwall/gjson"
)

// openCodeV2ResultContent retains native Tool.Content arrays as JSON. Flattening
// them through bodyContent would discard file URIs and their MIME/name metadata.
// This is also the reducer's canonical form, so inline and lifted results agree.
func openCodeV2ResultContent(body gjson.Result) (string, string) {
	if body.IsArray() {
		return body.Raw, "application/json"
	}
	return bodyContent(body)
}

// opencodeV2BodyFields lifts the native message representation released in
// OpenCode 2.0.29: assistant content tools and user file payloads. File references
// in tool results remain JSON; only user files carry inline binary data.
func opencodeV2BodyFields(e gjson.Result) []bodyField {
	if e.Get("type").String() != "message_v2" {
		return nil
	}
	var fields []bodyField
	data := e.Get("data")
	switch data.Get("type").String() {
	case "assistant":
		for _, part := range data.Get("content").Array() {
			if part.Get("type").String() != "tool" {
				continue
			}
			state := part.Get("state")
			input := state.Get("input")
			if f, ok := rawField(input, input.Raw, "application/json", bodyKindInput); ok {
				f.subagentType = openCodeV2InputSubagentType(input)
				fields = append(fields, f)
			}
			status := state.Get("status").String()
			if status != "completed" && status != "error" {
				continue
			}
			result := state.Get("content")
			if !result.Exists() && status == "error" {
				result = state.Get("error")
			}
			content, media := openCodeV2ResultContent(result)
			if f, ok := rawField(result, content, media, bodyKindResult); ok {
				fields = append(fields, f)
			}
		}
	case "user":
		for _, file := range data.Get("files").Array() {
			payload := file.Get("data")
			if payload.Type != gjson.String {
				continue // Existing CAS references and unknown values stay intact.
			}
			decoded, ok := decodeBase64Body(payload.String())
			if !ok {
				continue // Malformed payloads stay inline, never become partial blobs.
			}
			media := file.Get("mime").String()
			if media == "" {
				media = "application/octet-stream"
			}
			if f, ok := rawField(payload, string(decoded), media, bodyKindAttachment); ok {
				fields = append(fields, f)
			}
		}
	case "shell":
		result := data.Get("output.output")
		content, media := openCodeV2ResultContent(result)
		if f, ok := rawField(result, content, media, bodyKindResult); ok {
			fields = append(fields, f)
		}
	}
	// JSON member order is not schema order. Rewriting must advance in the
	// source's order even when content precedes input inside a tool state.
	sort.Slice(fields, func(i, j int) bool { return fields[i].start < fields[j].start })
	return fields
}

// locateOpenCodeV2 is the bounded-memory twin of opencodeV2BodyFields. Only tiny
// discriminators are buffered; tool payloads and arbitrary MIME files stream
// through the existing canonical readers. Decode, scan and emit failures reach
// the caller instead of producing a CAS reference to incomplete bytes.
func locateOpenCodeV2(s *lineSource, emit func(BodyLocation) error) error {
	typ, err := s.topType(Key("type"))
	if err != nil || typ != "message_v2" {
		return err
	}
	typ, err = s.unquotedAt([]Step{Key("data"), Key("type")})
	if err != nil {
		return err
	}
	switch typ {
	case "assistant":
		return WalkArrayElements(s.ctx, []Step{Key("data"), Key("content")}, nil, s.reader(),
			func(_ int, part ValueSpan, _ map[Step]ValueSpan) error {
				return s.locateOpenCodeV2Tool(part, emit)
			})
	case "user":
		return WalkArrayElements(s.ctx, []Step{Key("data"), Key("files")}, []Step{Key("data"), Key("mime")}, s.reader(),
			func(_ int, _ ValueSpan, subs map[Step]ValueSpan) error {
				payload, ok := subs[Key("data")]
				if !ok {
					return nil
				}
				head, err := s.readHead(payload)
				if err != nil || head != '"' {
					return err
				}
				media := ""
				if mime, ok := subs[Key("mime")]; ok {
					media, err = s.unquoted(mime)
					if err != nil {
						return err
					}
				}
				if media == "" {
					media = "application/octet-stream"
				}
				return emit(BodyLocation{Span: payload, Kind: BodyBase64, Media: media})
			})
	case "shell":
		spans, err := s.locate([][]Step{{Key("data"), Key("output"), Key("output")}})
		if err != nil {
			return err
		}
		if output, ok := spans[0]; ok {
			sentinel, err := s.openCodeV2Sentinel(output)
			if err != nil || sentinel {
				return err
			}
			loc, ok, err := s.classifyResult(output)
			if err != nil || !ok {
				return err
			}
			return emit(loc)
		}
	}
	return nil
}

func (s *lineSource) locateOpenCodeV2Tool(part ValueSpan, emit func(BodyLocation) error) error {
	spans, err := s.openCodeV2Spans(part, [][]Step{
		{Key("type")}, {Key("state"), Key("status")},
		{Key("state"), Key("input")}, {Key("state"), Key("content")}, {Key("state"), Key("error")},
	})
	if err != nil {
		return err
	}
	typ, err := s.unquoted(spans[0])
	if err != nil || typ != "tool" {
		return err
	}
	var locations []BodyLocation
	if input, ok := spans[2]; ok {
		sentinel, err := s.openCodeV2Sentinel(input)
		if err != nil {
			return err
		}
		if !sentinel {
			loc := BodyLocation{Span: input, Kind: BodyRaw, Media: "application/json"}
			if loc.FilePath, loc.Detail, err = s.inputProjections(input, loc.Kind, loc.Media); err != nil {
				return err
			}
			if loc.SubagentType, err = s.openCodeV2InputSubagentType(input); err != nil {
				return err
			}
			locations = append(locations, loc)
		}
	}
	status, err := s.unquoted(spans[1])
	if err != nil {
		return err
	}
	if status == "completed" || status == "error" {
		result, ok := spans[3]
		if !ok && status == "error" {
			result, ok = spans[4]
		}
		if ok {
			sentinel, err := s.openCodeV2Sentinel(result)
			if err != nil {
				return err
			}
			if !sentinel {
				head, err := s.readHead(result)
				if err != nil {
					return err
				}
				kind, media := ClassifyResultBody(head)
				if head == '[' {
					kind, media = BodyRaw, "application/json"
				}
				locations = append(locations, BodyLocation{Span: result, Kind: kind, Media: media})
			}
		}
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].Span.Start < locations[j].Span.Start })
	for _, loc := range locations {
		if err := emit(loc); err != nil {
			return err
		}
	}
	return nil
}

// openCodeV2Spans translates a scan within a tool back to line-relative spans.
func (s *lineSource) openCodeV2Spans(parent ValueSpan, paths [][]Step) (map[int]ValueSpan, error) {
	r := CanonicalBodyReader(s.ctx, s.f, s.base, parent, BodyRaw)
	located, err := LocateValues(s.ctx, paths, readerWindows(r))
	if err != nil {
		return nil, err
	}
	spans := make(map[int]ValueSpan, len(located))
	for _, item := range located {
		spans[item.PathIndex] = ValueSpan{Start: parent.Start + item.Span.Start, End: parent.Start + item.Span.End}
	}
	return spans, nil
}

func (s *lineSource) openCodeV2Sentinel(span ValueSpan) (bool, error) {
	head, err := s.readHead(span)
	if err != nil || head != '{' {
		return false, err
	}
	spans, err := s.openCodeV2Spans(span, [][]Step{{Key(sentinelKey)}})
	_, ok := spans[0]
	return ok, err
}

const maxOpenCodeV2SubagentType = 256

var openCodeV2SubagentKeys = []string{"agent", "subagent_type"}

func openCodeV2InputSubagentType(input gjson.Result) string {
	for _, key := range openCodeV2SubagentKeys {
		if v := input.Get(key); v.Type == gjson.String && len(v.String()) <= maxOpenCodeV2SubagentType && v.String() != "" {
			return v.String()
		}
	}
	return ""
}

func (s *lineSource) openCodeV2InputSubagentType(input ValueSpan) (string, error) {
	paths := [][]Step{{Key("agent")}, {Key("subagent_type")}}
	spans, err := s.openCodeV2Spans(input, paths)
	if err != nil {
		return "", err
	}
	for i := range paths {
		if span, ok := spans[i]; ok {
			relative := ValueSpan{Start: span.Start - input.Start, End: span.End - input.Start}
			value, ok, err := s.readStringSpan(input, BodyRaw, relative, maxOpenCodeV2SubagentType)
			if err != nil {
				return "", err
			}
			if ok && value != "" {
				return value, nil
			}
		}
	}
	return "", nil
}
