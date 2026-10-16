package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/upload"
	"github.com/jssblck/akari/internal/parser"
)

type selectedRuntimeEncoder struct{}

func (selectedRuntimeEncoder) EncodeBody(raw []byte) (string, []byte, string) {
	return parser.HashString(string(raw)), raw, parser.ContentRaw
}

// This opt-in test consumes ONLY the disposable corpus made by testdata/opencode-v2-corpus.py.
// It never discovers default roots or invokes the development seeder (which reads live histories).
func TestOpenCodeSelectedRuntimeIngestion(t *testing.T) {
	root := os.Getenv("AKARI_TEST_OPENCODE_V2_CORPUS")
	if root == "" {
		t.Skip("set AKARI_TEST_OPENCODE_V2_CORPUS to an explicitly synthetic selected-runtime corpus")
	}
	if os.Getenv("AKARI_TEST_DATABASE_URL") == "" {
		t.Fatal("selected-runtime certification requires disposable AKARI_TEST_DATABASE_URL")
	}
	var receipt struct {
		Version            string `json:"version"`
		BinarySHA256       string `json:"binary_sha256"`
		SourceCommit       string `json:"source_commit"`
		OwnedServerStopped bool   `json:"owned_server_stopped"`
		Corpora            []struct {
			Path   string   `json:"path"`
			SHA256 string   `json:"sha256"`
			IDs    []string `json:"ids"`
		} `json:"corpora"`
	}
	rawReceipt, err := os.ReadFile(filepath.Join(root, "corpus-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Version != "2.0.30" || receipt.BinarySHA256 != "ddb556472e044be6d757fbc5fefc25b2edd2896ddbc0d388d6178b7b0a639b96" || receipt.SourceCommit != "f16ef9ad8878279ea2be3cfab7baeed97aef61aa" || !receipt.OwnedServerStopped {
		t.Fatal("selected runtime evidence differs")
	}
	if len(receipt.Corpora) != 4 {
		t.Fatalf("corpora=%d want legacy, native, mixed and rich", len(receipt.Corpora))
	}
	// os.UserCacheDir uses HOME on macOS and XDG_CACHE_HOME on Linux.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv, st, worker := newTestServerWithReparse(t)
	client, owner := ingestClient(t, srv.URL, st)
	web := newClient(t)
	if status, body := postJSON(t, web, srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"hopper-1906"}`); status != http.StatusOK {
		t.Fatalf("login=%d %v", status, body)
	}
	ctx := context.Background()
	ids := map[string]bool{}
	bodies, messages, tools, usage, attachments := 0, 0, 0, 0, 0
	for i, corpus := range receipt.Corpora {
		wantName := []string{"legacy.sqlite", "native.sqlite", "mixed.sqlite", "rich.sqlite"}[i]
		if corpus.Path != wantName {
			t.Fatalf("corpus path=%q want %q", corpus.Path, wantName)
		}
		dbPath := filepath.Join(root, corpus.Path)
		original, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if sha256Hex(original) != corpus.SHA256 {
			t.Fatal("synthetic source digest differs")
		}
		files, _, err := discover.Discover([]discover.Root{{Agent: "opencode", Dir: root, SessionDB: corpus.Path}}, discover.Excluder{})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != len(corpus.IDs) {
			t.Fatalf("%s discovery=%d want %d (duplicate legacy or missing native)", corpus.Path, len(files), len(corpus.IDs))
		}
		var discoveredIDs []string
		for _, file := range files {
			sourceID := strings.TrimSuffix(filepath.Base(file.Path), ".jsonl")
			alreadyUploaded := ids[sourceID]
			if alreadyUploaded && (corpus.Path != "mixed.sqlite" || len(receipt.Corpora[0].IDs) != 1 || sourceID != receipt.Corpora[0].IDs[0]) {
				t.Fatalf("unexpected duplicate source id %s", sourceID)
			}
			ids[sourceID] = true
			discoveredIDs = append(discoveredIDs, sourceID)
			raw, err := os.ReadFile(file.Path)
			if err != nil {
				t.Fatal(err)
			}
			inline, err := parser.Parse(parser.AgentOpenCode, raw)
			if err != nil {
				t.Fatal(err)
			}
			_, expectedBodies, err := parser.ExtractBodies(parser.AgentOpenCode, raw, selectedRuntimeEncoder{})
			if err != nil {
				t.Fatal(err)
			}
			target := upload.Target{Agent: "opencode", Path: file.Path, SourceID: sourceID, Kind: "remote", ProjectKey: "github.com/jssblck/akari", Machine: "synthetic-selected-runtime", Finalize: true}
			wantAction := upload.ActionUploaded
			if alreadyUploaded {
				wantAction = upload.ActionReset
			}
			if out, err := client.SyncFile(ctx, target); err != nil || out.Action != wantAction {
				t.Fatalf("%s upload=%+v err=%v", sourceID, out, err)
			}
			if _, err := worker.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			var sid int64
			if err := st.Pool.QueryRow(ctx, "SELECT id FROM sessions WHERE user_id=$1 AND agent='opencode' AND source_session_id=$2", owner, sourceID).Scan(&sid); err != nil {
				t.Fatal(err)
			}
			transformed, err := parser.Parse(parser.AgentOpenCode, []byte(storedRaw(t, st, sid)))
			if err != nil {
				t.Fatal(err)
			}
			// Extraction changes line lengths and therefore byte offsets, not usage identity.
			inlineUsage := append([]parser.Usage(nil), inline.UsageEvent...)
			transformedUsage := append([]parser.Usage(nil), transformed.UsageEvent...)
			for i := range inlineUsage {
				inlineUsage[i].SourceOffset = 0
			}
			for i := range transformedUsage {
				transformedUsage[i].SourceOffset = 0
			}
			if !reflect.DeepEqual(inline.Messages, transformed.Messages) || !reflect.DeepEqual(inlineUsage, transformedUsage) || !reflect.DeepEqual(inline.Identity, transformed.Identity) || !reflect.DeepEqual(inline.Events, transformed.Events) {
				t.Fatalf("%s lost text/reasoning/usage/identity/events on extraction", sourceID)
			}
			if len(inline.ToolCalls) != len(transformed.ToolCalls) || len(inline.Attachments) != len(transformed.Attachments) {
				t.Fatalf("%s lost tool/attachment records", sourceID)
			}
			for i, want := range inline.ToolCalls {
				got := transformed.ToolCalls[i]
				// Body bytes are checked independently through stored references and HTTP below.
				want.InputJSON = ""
				got.InputJSON = ""
				want.InputSHA256 = ""
				got.InputSHA256 = ""
				want.InputBytes = 0
				got.InputBytes = 0
				want.InputMediaType = ""
				got.InputMediaType = ""
				want.ResultBody = ""
				got.ResultBody = ""
				want.ResultSHA256 = ""
				got.ResultSHA256 = ""
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("%s tool metadata differs: %+v vs %+v", sourceID, want, got)
				}
			}
			for i, want := range inline.Attachments {
				got := transformed.Attachments[i]
				want.Content = ""
				got.Content = ""
				want.SHA256 = ""
				got.SHA256 = ""
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("%s attachment metadata differs", sourceID)
				}
			}

			for table, want := range map[string]int{"messages": len(inline.Messages), "tool_calls": len(inline.ToolCalls), "usage_events": len(inline.UsageEvent), "attachments": len(inline.Attachments)} {
				var got int
				if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE session_id=$1", sid).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("%s %s=%d want %d", sourceID, table, got, want)
				}
			}
			for _, body := range expectedBodies {
				// Small fixed fixture bodies stay raw; both stored and HTTP served bytes must match.
				assertBlob(t, st, body.SHA256, string(body.Stored), body.MediaType)
				var refs int
				if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tool_calls WHERE session_id=$1 AND (input_sha256=$2 OR result_sha256=$2)) + (SELECT count(*) FROM attachments WHERE session_id=$1 AND sha256=$2)`, sid, body.SHA256).Scan(&refs); err != nil {
					t.Fatal(err)
				}
				if refs == 0 {
					t.Fatalf("%s body %s uploaded but not referenced", sourceID, body.SHA256)
				}
				response, err := web.Get(fmt.Sprintf("%s/api/v1/session/%d/blob/%s", srv.URL, sid, body.SHA256))
				if err != nil {
					t.Fatal(err)
				}
				served, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != 200 || !bytes.Equal(served, body.Stored) || !strings.HasPrefix(response.Header.Get("Content-Type"), body.MediaType) {
					t.Fatalf("%s body HTTP readback differs: status=%d err=%v", sourceID, response.StatusCode, err)
				}
				bodies++
			}
			if out, err := client.SyncFile(ctx, target); err != nil || out.Action != upload.ActionUpToDate || out.UploadedBytes != 0 {
				t.Fatalf("%s duplicate sync=%+v err=%v", sourceID, out, err)
			}
			messages += len(inline.Messages)
			tools += len(inline.ToolCalls)
			usage += len(inline.UsageEvent)
			attachments += len(inline.Attachments)
		}
		sort.Strings(discoveredIDs)
		if !reflect.DeepEqual(discoveredIDs, corpus.IDs) {
			t.Fatalf("%s source IDs differ: %v", corpus.Path, discoveredIDs)
		}
		after, err := os.ReadFile(dbPath)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("discovery changed synthetic DB %s", corpus.Path)
		}
	}
	if len(ids) != 8 || messages != 16 || bodies != 20 || tools != 12 || usage != 3 || attachments != 2 {
		t.Fatalf("coverage differs: sessions=%d messages=%d bodies=%d tools=%d usage=%d attachments=%d", len(ids), messages, bodies, tools, usage, attachments)
	}
	var storedCount int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id=$1", owner).Scan(&storedCount); err != nil {
		t.Fatal(err)
	}
	if storedCount != 8 {
		t.Fatalf("stored IDs=%d want 8 after legacy-to-native replacement", storedCount)
	}
	var parent, child int64
	var role, relationship string
	if err := st.Pool.QueryRow(ctx, `SELECT p.id,c.parent_session_id,c.subagent_name,c.relationship_type FROM sessions c JOIN sessions p ON p.source_session_id='ses_akari_a1_parent' AND p.user_id=c.user_id WHERE c.source_session_id='ses_akari_a1_child'`).Scan(&parent, &child, &role, &relationship); err != nil {
		t.Fatal(err)
	}
	if parent != child || role != "explore" || relationship != "subagent" {
		t.Fatalf("child link/role=%d %d %q %q", parent, child, role, relationship)
	}
	var text, thinking, model string
	var input, output, reasoning, read, write int
	var cost float64
	if err := st.Pool.QueryRow(ctx, `SELECT m.content,m.thinking_text,m.model,u.input_tokens,u.output_tokens,u.reasoning_tokens,u.cache_read_tokens,u.cache_write_tokens,u.cost_usd FROM messages m JOIN sessions s ON s.id=m.session_id JOIN usage_events u ON u.session_id=s.id WHERE s.source_session_id='ses_akari_a1_child' AND m.role='assistant'`).Scan(&text, &thinking, &model, &input, &output, &reasoning, &read, &write, &cost); err != nil {
		t.Fatal(err)
	}
	if text != "A1 answer retained" || thinking != "A1 reasoning retained" || model != "openai/gpt-6-sol" || input != 10 || output != 20 || reasoning != 3 || read != 4 || write != 5 || cost != 0.012 {
		t.Fatalf("stored rich projection differs: %q %q %q %d %d %d %d %d %g", text, thinking, model, input, output, reasoning, read, write, cost)
	}
	t.Logf("selected stock2.0.30 discovery/upload/rebuild/HTTP readback: %d IDs, %d messages, %d tools, %d usage rows, %d attachments, %d bodies", len(ids), messages, tools, usage, attachments, bodies)
}
