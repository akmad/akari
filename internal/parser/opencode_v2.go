package parser

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ValidateOpenCodeV2Message guards both discovery and the reducer against silently
// projecting an unfamiliar transcript shape. Native data follows the tagged union
// in OpenCode 2.0.29 packages/schema/src/session-message.ts.
func ValidateOpenCodeV2Message(raw []byte) error {
	if !gjson.ValidBytes(raw) {
		return fmt.Errorf("invalid JSON")
	}
	d := gjson.ParseBytes(raw)
	if !d.IsObject() {
		return fmt.Errorf("message data must be an object")
	}
	// Only explicitly marked references are interpreted as CAS values. Native
	// input objects with similar ordinary fields retain their normal meaning.
	refs := []gjson.Result{d.Get("error"), d.Get("output.output")}
	for _, file := range d.Get("files").Array() {
		refs = append(refs, file.Get("data"))
	}
	for _, content := range d.Get("content").Array() {
		if content.Get("type").String() == "tool" {
			refs = append(refs, content.Get("state.input"), content.Get("state.content"), content.Get("state.error"))
		}
	}
	for _, ref := range refs {
		if err := validateOpenCodeV2BodyRef(ref); err != nil {
			return err
		}
	}

	require := func(path string, kind gjson.Type) error {
		v := d.Get(path)
		if !v.Exists() || v.Type != kind {
			return fmt.Errorf("invalid or missing %s", path)
		}
		return nil
	}
	if err := require("time.created", gjson.Number); err != nil {
		return err
	}
	for _, path := range []string{"time.created", "time.completed"} {
		if v := d.Get(path); v.Exists() && (v.Type != gjson.Number || v.Float() < 0 || math.IsInf(v.Float(), 0) || v.Float() >= float64(math.MaxInt64) || v.Float() != math.Trunc(v.Float())) {
			return fmt.Errorf("invalid %s", path)
		}
	}
	switch d.Get("type").String() {
	case "user":
		if err := require("text", gjson.String); err != nil {
			return err
		}
		for _, key := range []string{"files", "agents", "skills"} {
			if a := d.Get(key); a.Exists() && !a.IsArray() {
				return fmt.Errorf("%s must be an array", key)
			}
		}
		for _, f := range d.Get("files").Array() {
			if f.Get("mime").Type != gjson.String {
				return fmt.Errorf("file mime is missing")
			}
			if _, ok := asCASRef(f.Get("data")); !ok {
				if f.Get("data").Type != gjson.String {
					return fmt.Errorf("file data is missing")
				}
				if _, err := base64.StdEncoding.DecodeString(f.Get("data").String()); err != nil {
					return fmt.Errorf("invalid file base64")
				}
			}
		}
	case "assistant":
		if err := require("agent", gjson.String); err != nil {
			return err
		}
		if !validOpenCodeV2Model(d.Get("model")) {
			return fmt.Errorf("invalid assistant model")
		}
		if !d.Get("content").IsArray() {
			return fmt.Errorf("assistant content must be an array")
		}
		for _, c := range d.Get("content").Array() {
			switch c.Get("type").String() {
			case "text", "reasoning":
				if c.Get("text").Type != gjson.String {
					return fmt.Errorf("invalid %s text", c.Get("type").String())
				}
			case "tool":
				if c.Get("id").Type != gjson.String || c.Get("name").Type != gjson.String {
					return fmt.Errorf("invalid tool identity")
				}
				state := c.Get("state")
				if name := c.Get("name").String(); name == "subagent" || name == "task" {
					if _, referenced := asCASRef(state.Get("input")); !referenced {
						for _, key := range openCodeV2SubagentKeys {
							if role := state.Get("input." + key); role.Exists() && (role.Type != gjson.String || len(role.String()) > maxOpenCodeV2SubagentType) {
								return fmt.Errorf("invalid subagent role %s", key)
							}
						}
					}
				}
				switch state.Get("status").String() {
				case "streaming":
					if state.Get("input").Type != gjson.String {
						if _, ok := asCASRef(state.Get("input")); !ok {
							return fmt.Errorf("invalid streaming tool input")
						}
					}
				case "running", "completed", "error":
					if !state.Get("input").IsObject() {
						return fmt.Errorf("invalid tool input")
					}
				default:
					return fmt.Errorf("unsupported tool status %q", state.Get("status").String())
				}
				if state.Get("status").String() == "completed" && !state.Get("content").Exists() {
					return fmt.Errorf("completed tool content missing")
				}
				if content := state.Get("content"); content.Exists() {
					if err := validateOpenCodeV2ToolContent(content); err != nil {
						return err
					}
				}
				if state.Get("status").String() == "error" && !validOpenCodeV2Error(state.Get("error")) {
					return fmt.Errorf("invalid tool error")
				}
			default:
				return fmt.Errorf("unsupported assistant content %q", c.Get("type").String())
			}
		}
	case "synthetic", "system", "skill":
		if err := require("text", gjson.String); err != nil {
			return err
		}
	case "model-switched":
		if !validOpenCodeV2Model(d.Get("model")) {
			return fmt.Errorf("invalid switched model")
		}
	case "agent-switched":
		if err := require("agent", gjson.String); err != nil {
			return err
		}
	case "location-switched":
		if err := require("location.directory", gjson.String); err != nil {
			return err
		}
	case "shell":
		if err := require("command", gjson.String); err != nil {
			return err
		}
		switch d.Get("status").String() {
		case "running", "exited", "timeout", "killed":
		default:
			return fmt.Errorf("unsupported shell status")
		}
		if o := d.Get("output"); o.Exists() && o.Get("output").Type != gjson.String {
			if _, ok := asCASRef(o.Get("output")); !ok {
				return fmt.Errorf("invalid shell output")
			}
		}
	case "compaction":
		switch d.Get("status").String() {
		case "running", "completed":
			if err := require("summary", gjson.String); err != nil {
				return err
			}
			if err := require("recent", gjson.String); err != nil {
				return err
			}
		case "failed":
			if !validOpenCodeV2Error(d.Get("error")) {
				return fmt.Errorf("invalid compaction error")
			}
		default:
			return fmt.Errorf("unsupported compaction status")
		}
	case "idle":
		switch d.Get("outcome").String() {
		case "succeeded", "failed", "interrupted":
		default:
			return fmt.Errorf("unsupported idle outcome")
		}
	default:
		return fmt.Errorf("unsupported message type %q", d.Get("type").String())
	}
	if e := d.Get("error"); e.Exists() && !validOpenCodeV2Error(e) {
		return fmt.Errorf("invalid message error")
	}
	if t := d.Get("tokens"); t.Exists() {
		for _, p := range []string{"input", "output", "reasoning", "cache.read", "cache.write"} {
			v := t.Get(p)
			if v.Type != gjson.Number || v.Float() < 0 || math.IsInf(v.Float(), 0) || v.Float() >= float64(math.MaxInt64) || v.Float() != math.Trunc(v.Float()) {
				return fmt.Errorf("invalid tokens.%s", p)
			}
		}
	}
	if c := d.Get("cost"); c.Exists() && (c.Type != gjson.Number || c.Float() < 0 || math.IsInf(c.Float(), 0)) {
		return fmt.Errorf("invalid cost")
	}
	return nil
}

func validOpenCodeV2Model(m gjson.Result) bool {
	return m.IsObject() && m.Get("id").Type == gjson.String && m.Get("providerID").Type == gjson.String
}
func validOpenCodeV2Error(e gjson.Result) bool {
	if _, ok := asCASRef(e); ok {
		return true
	}
	return e.IsObject() && e.Get("type").Type == gjson.String && e.Get("message").Type == gjson.String
}
func validateOpenCodeV2ToolContent(c gjson.Result) error {
	if _, ok := asCASRef(c); ok {
		return nil
	}
	if !c.IsArray() || len(c.Array()) == 0 {
		return fmt.Errorf("tool content must be a nonempty array")
	}
	for _, p := range c.Array() {
		switch p.Get("type").String() {
		case "text":
			if p.Get("text").Type != gjson.String {
				return fmt.Errorf("invalid tool text")
			}
		case "file":
			if p.Get("uri").Type != gjson.String || p.Get("mime").Type != gjson.String {
				return fmt.Errorf("invalid tool file")
			}
		default:
			return fmt.Errorf("unsupported tool content %q", p.Get("type").String())
		}
	}
	return nil
}

func (r *reducer) reduceOpenCodeV2Message(e gjson.Result, offset int64) error {
	d := e.Get("data")
	if err := ValidateOpenCodeV2Message([]byte(d.Raw)); err != nil {
		return fmt.Errorf("OpenCode v2 message %s: %w", e.Get("id").String(), err)
	}
	ts := openCodeMessageTime(d, e)
	r.observe(ts)
	r.observe(parseUnixMilli(d.Get("time.completed").Int()))
	r.closeTurn()
	typ := d.Get("type").String()
	switch typ {
	case "user":
		ord := r.addUser(d.Get("text").String(), ts)
		for _, f := range d.Get("files").Array() {
			r.addOpenCodeV2Attachment(ord, f)
		}
		for _, key := range []string{"agents", "skills"} {
			for _, a := range d.Get(key).Array() {
				r.addEvent("opencode_"+key, map[string]any{"attachment": a.Value()}, ts)
			}
		}
	case "assistant":
		ord := r.nextOrdinal
		r.nextOrdinal++
		model := providerModel(d.Get("model.providerID").String(), d.Get("model.id").String())
		m := MessageOp{Ordinal: ord, Role: RoleAssistant, Model: model, Timestamp: ts}
		var text, thinking []string
		callIndex := 0
		for _, c := range d.Get("content").Array() {
			switch c.Get("type").String() {
			case "text":
				text = append(text, c.Get("text").String())
			case "reasoning":
				m.HasThinking = true
				t := c.Get("text").String()
				thinking = append(thinking, t)
				m.ThinkingBytes += len(t)
				if t == "" {
					m.ThinkingBytes += openCodeV2ReasoningWeight(c.Get("state"))
				}
			case "tool":
				m.HasToolUse = true
				r.openCodeV2Tool(ord, callIndex, c)
				callIndex++
			}
		}
		m.Content = strings.Join(text, "\n")
		m.ThinkingText = strings.Join(thinking, "\n")
		r.d.Messages = append(r.d.Messages, m)
		r.openCodeV2Usage(d, model, &ord, e.Get("id").String(), ts, offset)
		if errv := d.Get("error"); errv.Exists() {
			r.addEvent(EventAPIError, map[string]any{"message": errv.Get("message").String(), "error": errv.Value()}, ts)
		}
		if retry := d.Get("retry"); retry.Exists() {
			r.addEvent(EventAPIError, map[string]any{"retry_attempt": retry.Get("attempt").Int(), "error": retry.Get("error").Value()}, ts)
		}
	case "synthetic", "system", "skill":
		r.addContext(d.Get("text").String(), ts)
	case "model-switched":
		r.model = providerModel(d.Get("model.providerID").String(), d.Get("model.id").String())
		attrs := map[string]any{"provider": d.Get("model.providerID").String(), "model": d.Get("model.id").String()}
		if variant := d.Get("model.variant").String(); variant != "" {
			attrs["variant"] = variant
		}
		r.addEvent(EventModelChange, attrs, ts)
	case "location-switched":
		r.d.Cwd = d.Get("location.directory").String()
		r.addEvent("opencode_location_change", map[string]any{"location": d.Get("location").Value()}, ts)
	case "agent-switched":
		r.addEvent("opencode_agent_change", map[string]any{"agent": d.Get("agent").String()}, ts)
	case "idle":
		kind := EventTurnEnd
		if d.Get("outcome").String() == "interrupted" {
			kind = EventTurnAborted
		}
		r.addEvent(kind, map[string]any{"outcome": d.Get("outcome").String(), "error": d.Get("error").Value()}, ts)
	case "compaction":
		r.addEvent(EventCompaction, map[string]any{"status": d.Get("status").String(), "reason": d.Get("reason").String(), "error": d.Get("error").Value()}, ts)
		if d.Get("status").String() == "completed" {
			r.addContext(d.Get("summary").String()+"\n"+d.Get("recent").String(), ts)
		}
		r.openCodeV2Usage(d, providerModel(d.Get("model.providerID").String(), d.Get("model.id").String()), nil, e.Get("id").String(), ts, offset)
	case "shell":
		ord := r.ensureAssistant(ts)
		input, _ := json.Marshal(map[string]string{"command": d.Get("command").String()})
		tc := ToolCall{MessageOrdinal: ord, CallIndex: 0, ToolName: "bash", Category: toolCategory("bash"), CallUID: e.Get("id").String()}
		setToolInput(&tc, gjson.ParseBytes(input), "application/json")
		r.d.ToolCalls = append(r.d.ToolCalls, tc)
		r.open.HasToolUse = true
		if d.Get("status").String() != "running" {
			out := d.Get("output")
			if _, ok := asCASRef(out); !ok {
				out = out.Get("output")
			}
			r.applyResult(tc.CallUID, out, d.Get("status").String() != "exited" || d.Get("exit").Int() != 0)
		}
		r.closeTurn()
	}
	return nil
}

func (r *reducer) openCodeV2Usage(d gjson.Result, model string, ord *int, id string, ts time.Time, offset int64) {
	t := d.Get("tokens")
	cost := d.Get("cost")
	if !t.Exists() && !cost.Exists() {
		return
	}
	u := Usage{MessageOrdinal: ord, Model: model, Input: int(t.Get("input").Int()), Output: int(t.Get("output").Int()), Reasoning: int(t.Get("reasoning").Int()), CacheRead: int(t.Get("cache.read").Int()), CacheWrite: int(t.Get("cache.write").Int()), OccurredAt: ts, DedupKey: id}
	if cost.Exists() {
		v := cost.Float()
		u.ReportedCostUSD = &v
	}
	r.addUsage(u, offset)
}

func (r *reducer) openCodeV2Tool(ord, callIndex int, c gjson.Result) {
	state := c.Get("state")
	tc := ToolCall{MessageOrdinal: ord, CallIndex: callIndex, ToolName: c.Get("name").String(), Category: toolCategory(c.Get("name").String()), CallUID: c.Get("id").String(), FilePath: toolFilePath(state.Get("input"))}
	setToolInput(&tc, state.Get("input"), "application/json")
	r.d.ToolCalls = append(r.d.ToolCalls, tc)
	status := state.Get("status").String()
	if status != "completed" && status != "error" {
		return
	}
	body := state.Get("content")
	if !body.Exists() {
		body = state.Get("error")
	}
	result := ToolResultOp{CallUID: tc.CallUID, Status: "ok"}
	if status == "error" {
		result.Status = "error"
	}
	if ref, ok := asCASRef(body); ok {
		result.BodySHA256, result.Bytes, result.MediaType = ref.SHA256, ref.Bytes, ref.MediaType
	} else {
		result.Body, result.MediaType = openCodeV2ResultContent(body)
		result.Bytes = len(result.Body)
	}
	r.d.ToolResults = append(r.d.ToolResults, result)
	if tc.ToolName == "task" || tc.ToolName == "subagent" {
		if child := openCodeV2ChildID(state); child != "" {
			r.addEvent(EventSubagentActivity, map[string]any{"child_session_id": child, "state": status, "subagent_type": openCodeV2SubagentType(state), "description": tc.Detail}, parseUnixMilli(c.Get("time.created").Int()))
		}
	}
}

func (r *reducer) addOpenCodeV2Attachment(ord int, f gjson.Result) {
	a := AttachmentOp{MessageOrdinal: ord, Filename: f.Get("name").String(), MediaType: f.Get("mime").String()}
	if ref, ok := asCASRef(f.Get("data")); ok {
		a.SHA256, a.Bytes, a.MediaType = ref.SHA256, ref.Bytes, ref.MediaType
	} else {
		b, _ := base64.StdEncoding.DecodeString(f.Get("data").String())
		a.Content = string(b)
		a.Bytes = len(b)
	}
	r.d.Attachments = append(r.d.Attachments, a)
}

// V1 migrated task tools retain sessionId; native subagent tools report sessionID.
func openCodeV2ChildID(state gjson.Result) string {
	if id := state.Get("metadata.sessionID").String(); id != "" {
		return id
	}
	return state.Get("metadata.sessionId").String()
}

func openCodeV2SubagentType(state gjson.Result) string {
	if ref, ok := asCASRef(state.Get("input")); ok {
		return ref.SubagentType
	}
	return openCodeV2InputSubagentType(state.Get("input"))
}

// Native messages keep the producing adapter's unwrapped state; migrated v1
// messages retain the original provider-keyed metadata. Prefer one representation
// so a transcript carrying both is not counted twice.
func openCodeV2ReasoningWeight(state gjson.Result) int {
	for _, path := range []string{"reasoningEncryptedContent", "signature", "redactedData", "xai.reasoningEncryptedContent"} {
		if value := state.Get(path); value.Type == gjson.String && value.String() != "" {
			return len(value.String())
		}
	}
	return 0
}

func validateOpenCodeV2BodyRef(v gjson.Result) error {
	if !v.IsObject() || !v.Get(sentinelKey).Exists() {
		return nil
	}
	invalid := func() error { return fmt.Errorf("invalid body reference") }
	marker := v.Get(sentinelKey)
	if marker.Type != gjson.Number || marker.Float() != 1 {
		return invalid()
	}
	sha := v.Get("sha256")
	if sha.Type != gjson.String || len(sha.String()) != 64 {
		return invalid()
	}
	if _, err := hex.DecodeString(sha.String()); err != nil {
		return invalid()
	}
	n := v.Get("bytes")
	if n.Type != gjson.Number || n.Float() < 0 || math.IsInf(n.Float(), 0) || n.Float() >= float64(math.MaxInt64) || n.Float() != math.Trunc(n.Float()) {
		return invalid()
	}
	media := v.Get("media_type")
	if media.Type != gjson.String || media.String() == "" {
		return invalid()
	}
	for key, limit := range map[string]int{"file_path": maxSentinelFilePath, "detail": maxSentinelDetail, "subagent_type": maxOpenCodeV2SubagentType} {
		if field := v.Get(key); field.Exists() && (field.Type != gjson.String || len(field.String()) > limit) {
			return invalid()
		}
	}
	if status := v.Get("status"); status.Exists() && (status.Type != gjson.String || status.String() != "error") {
		return invalid()
	}
	return nil
}
