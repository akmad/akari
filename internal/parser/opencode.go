package parser

import "github.com/tidwall/gjson"

// OpenCode keeps its transcripts in a SQLite database rather than a file, so the
// akari client materializes one deterministic JSONL file per session (see
// internal/client/opencode) and the pipeline treats it like any other agent's
// on-disk transcript. This reducer reads that synthetic format, which is a
// faithful, lossless projection of the three tables it comes from:
//
//	{"type":"session","id":…,"slug":…,"title":…,"directory":…,"parentID":…,
//	 "agent":…,"model":{"id":…,"providerID":…,"variant":…},"version":…,"timeCreated":…}
//	{"type":"message","id":"msg_…","role":"user|assistant","timeCreated":…,"data":{…}}
//	{"type":"part","id":"prt_…","messageID":"msg_…","timeCreated":…,"data":{…}}
//
// The message line's `data` and the part line's `data` are the verbatim JSON
// OpenCode stored, so anything this reducer does not read today is still on the
// server and can be read by a later epoch without re-uploading.
//
// A message line opens a turn and every part line that follows folds into it, the
// same shape as the Codex reducer: OpenCode splits one turn's text, reasoning and
// tool calls across sibling `part` rows, so the fold is what turns them back into
// a single message row. A turn is closed by the next message line or by Finish.
//
// Timestamps are epoch milliseconds throughout (OpenCode stores integers, not
// RFC3339 strings), so this file uses parseMillis rather than parseTime.
func (r *reducer) reduceOpencode(region []byte, base int64) error {
	return eachLine(region, base, func(line []byte, offset int64) error {
		if !gjson.ValidBytes(line) {
			return nil
		}
		e := gjson.ParseBytes(line)
		switch e.Get("type").String() {
		case "session":
			r.opencodeSession(e)
		case "message":
			r.opencodeMessage(e, offset)
		case "part":
			r.opencodePart(e)
		}
		return nil
	})
}

// opencodeSession reads the synthetic header line: the session's working
// directory and everything the sessions row records about the session's identity.
// OpenCode has no equivalent of Claude's gitBranch (its workspace table is empty
// on the observed corpus), so GitBranch is deliberately left unset rather than
// guessed at from the directory.
func (r *reducer) opencodeSession(e gjson.Result) {
	if dir := e.Get("directory").String(); dir != "" {
		r.d.Cwd = dir
	}
	r.observe(parseMillis(e.Get("timeCreated").Int()))
	if v := e.Get("title").String(); v != "" {
		r.d.Identity.CustomTitle = v
	}
	if v := e.Get("slug").String(); v != "" {
		r.d.Identity.Slug = v
	}
	// OpenCode's per-session `agent` is the role the session ran as ("build",
	// "explore", "plan"): for a child session that is exactly the subagent name,
	// and for a root session it is the mode it was driven in. Recording it on both
	// is harmless (the column is only read as a subagent label when a parent
	// exists) and keeps the field a straight copy of what the transcript declared.
	if v := e.Get("agent").String(); v != "" {
		r.d.Identity.SubagentName = v
	}
	if v := e.Get("parentID").String(); v != "" {
		r.d.Identity.ParentSourceID = v
	}
	// The model's `variant` is OpenCode's reasoning-effort knob ("high", "low"),
	// the same axis Codex records as reasoning effort.
	if v := e.Get("model.variant").String(); v != "" {
		r.d.Identity.ReasoningEffort = v
	}
}

// opencodeMessage opens a new turn. Every OpenCode message row is one turn: a user
// prompt or one assistant response, with its text, reasoning and tool calls
// following as part lines. The token counts ride on the message itself (its
// step-finish part repeats them, so summing the parts would double count), and its
// completion time yields the turn's duration telemetry.
func (r *reducer) opencodeMessage(e gjson.Result, offset int64) {
	r.closeTurn()
	d := e.Get("data")
	ts := parseMillis(e.Get("timeCreated").Int())
	if ts.IsZero() {
		ts = parseMillis(d.Get("time.created").Int())
	}
	r.observe(ts)

	role := RoleUser
	if d.Get("role").String() == "assistant" {
		role = RoleAssistant
	}
	ord := r.nextOrdinal
	r.nextOrdinal++
	r.openCalls = 0
	op := &MessageOp{Ordinal: ord, Role: role, Timestamp: ts}
	if role == RoleAssistant {
		// The bare model id, with no provider prefix: it is exactly the key the
		// pricing table uses ("gpt-5.6-sol"), and the provider is a routing detail.
		op.Model = d.Get("modelID").String()
	}
	r.open = op

	if role != RoleAssistant {
		return
	}

	if u := d.Get("tokens"); u.Exists() {
		o := ord
		r.addUsage(Usage{
			MessageOrdinal: &o, Model: op.Model,
			Input:      int(u.Get("input").Int()),
			Output:     int(u.Get("output").Int()),
			Reasoning:  int(u.Get("reasoning").Int()),
			CacheRead:  int(u.Get("cache.read").Int()),
			CacheWrite: int(u.Get("cache.write").Int()),
			OccurredAt: ts,
			// The message id is OpenCode's own per-turn key and is unique across the
			// database, so it dedups usage without needing the source offset.
			DedupKey: e.Get("id").String(),
		}, offset)
	}

	// A completed turn records how long it ran. OpenCode logs both ends in
	// milliseconds, so the duration is exact rather than sampled.
	created, completed := d.Get("time.created").Int(), d.Get("time.completed").Int()
	if created > 0 && completed >= created {
		r.addEvent(EventTurnEnd, map[string]any{"duration_ms": completed - created}, parseMillis(completed))
	}
	// A turn whose API call failed carries the provider's error verbatim.
	if err := d.Get("error"); err.Exists() {
		r.addEvent(EventAPIError, map[string]any{
			"message": opencodeErrorMessage(err),
		}, ts)
	}
}

// opencodeErrorMessage renders a message's error object as a one-line summary. The
// stored shape is {"name":…,"data":{"message":…}}; the inner message is the useful
// half, and the name is the fallback for an error that carries no message.
func opencodeErrorMessage(err gjson.Result) string {
	if m := err.Get("data.message").String(); m != "" {
		return m
	}
	if m := err.Get("message").String(); m != "" {
		return m
	}
	return err.Get("name").String()
}

// opencodePart folds one part row into the open turn. OpenCode's part types map
// onto the fold's three collectors: `text` is the turn's visible content (for a
// user message as much as an assistant one, since a prompt's body is a text part
// too), `reasoning` is its thinking, `tool` is a call plus its already-resolved
// result, and `file` is a pasted attachment. `step-start`, `step-finish` and
// `patch` carry no projected content: step-finish only repeats the message's own
// token counts, and patch is a snapshot pointer.
func (r *reducer) opencodePart(e gjson.Result) {
	if r.open == nil {
		return // a part with no message before it; nothing to fold into
	}
	d := e.Get("data")
	switch d.Get("type").String() {
	case "text":
		r.addOpenContent(d.Get("text").String())
	case "reasoning":
		r.addOpenReasoning(d.Get("text").String(), opencodeThinkingWeight(d))
	case "tool":
		r.opencodeTool(d)
	case "file":
		r.addAttachment(r.open.Ordinal, d.Get("url"), d.Get("filename").String())
	}
}

// opencodeThinkingWeight measures a reasoning part's reasoning-trace weight (see
// Message.ThinkingBytes). OpenCode keeps a short plaintext summary AND, for
// providers that return one, the opaque encrypted reasoning blob. The blob is the
// better proxy for how much the model actually reasoned (the plaintext is a
// one-line heading), so it wins when present and the plaintext is the fallback,
// matching how the Codex reducer weighs encrypted_content.
func opencodeThinkingWeight(d gjson.Result) int {
	if enc := d.Get("metadata.openai.reasoningEncryptedContent"); enc.Exists() {
		return len(enc.String())
	}
	return len(d.Get("text").String())
}

// opencodeTool records one tool part: the call and, because OpenCode rewrites the
// part in place as the tool runs, the result it already resolved to. The client
// only materializes a message once every one of its tool parts has settled, so a
// part reaching the server is always terminal (completed or error).
func (r *reducer) opencodeTool(d gjson.Result) {
	ord := r.open.Ordinal
	r.open.HasToolUse = true
	name := d.Get("tool").String()
	tc := ToolCall{
		MessageOrdinal: ord, CallIndex: r.openCalls,
		ToolName: name, Category: toolCategory(name),
		FilePath: d.Get("state.input.file_path").String(),
		CallUID:  d.Get("callID").String(),
	}
	setToolInput(&tc, d.Get("state.input"), "application/json")
	r.d.ToolCalls = append(r.d.ToolCalls, tc)
	r.openCalls++

	switch d.Get("state.status").String() {
	case "completed":
		r.applyResult(tc.CallUID, d.Get("state.output"), false)
	case "error":
		// A failed call carries no output; its state.error is the message the model
		// saw in the output's place, so that is the body recorded for the call.
		body := d.Get("state.output")
		if !body.Exists() {
			body = d.Get("state.error")
		}
		r.applyResult(tc.CallUID, body, true)
	}
}
