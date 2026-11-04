package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testControlToken = "control-token-for-tests-0123456789"

type ctlEnv struct {
	g   *Guard
	srv *httptest.Server
	con *fakeConsole
	mcp *fakeMCP
	dir string
}

func newCtlEnv(t *testing.T, mode Mode) *ctlEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		Mode: mode, DataDir: dir, ControlToken: testControlToken, DiscordAllow: defaultDiscordAllow,
		ChatMuteCmd: "mute {name} 10m spam", TrustedPlayers: map[string]bool{"trusty": true},
		MapIDs: map[string]string{"main": "overworld"},
	}
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	con := &fakeConsole{}
	g := NewGuard(cfg, nil, st, &Ops{C: con}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fm := &fakeMCP{reply: func(tool string, args map[string]any) (string, bool) {
		if tool == "discord_search_tools" {
			return `{"tools":[{"toolName":"discord_get_channel"},{"toolName":"discord_delete_channel"},{"toolName":"discord_create_message"}]}`, false
		}
		return `{"id":"1"}`, false
	}}
	mcpSrv := httptest.NewServer(fm)
	t.Cleanup(mcpSrv.Close)
	g.reporter = NewReporter(NewMCP(mcpSrv.URL, "2025-11-25", true), "TOKEN", "100", "55")
	ctl, err := NewControl(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ctl.Handler())
	t.Cleanup(srv.Close)
	return &ctlEnv{g: g, srv: srv, con: con, mcp: fm, dir: dir}
}

func (e *ctlEnv) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testControlToken)
	req.Header.Set("X-Toro-User", "admin1 (id 42)")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *ctlEnv) putRecord(t *testing.T, r *Record) {
	t.Helper()
	dir := filepath.Join(e.dir, "reports", r.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "report.md"), []byte("**ワールド**: main"), 0o644)
	for _, n := range r.Images {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("PNGDATA"), 0o644)
	}
}

func TestControlRequiresToken(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	for _, auth := range []string{"", "Bearer wrong", testControlToken, "Bearer " + testControlToken + "x"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/status", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("auth %q: status %d, want 401", auth, res.StatusCode)
		}
	}
	if code, _ := e.do(t, "GET", "/v1/status", ""); code != 200 {
		t.Fatalf("status %d", code)
	}
}

func TestControlConfigRejectsShortToken(t *testing.T) {
	t.Setenv("CONTROL_TOKEN", "short")
	if _, err := loadConfig(); err == nil {
		t.Fatal("want error for a short CONTROL_TOKEN")
	}
}

func TestControlIncidentsListGetImage(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	now := time.Now().Unix()
	e.putRecord(t, &Record{ID: "a-old", Kind: "block", Name: "old", Created: now - 100, State: StatePending, Images: []string{"damage.png"}})
	e.putRecord(t, &Record{ID: "b-new", Kind: "chat", Name: "new", Created: now, State: StatePending})
	e.putRecord(t, &Record{ID: "c-done", Kind: "block", Name: "done", Created: now - 50, State: StateReported})

	code, b := e.do(t, "GET", "/v1/incidents", "")
	var list struct{ Incidents []map[string]any }
	_ = json.Unmarshal(b, &list)
	if code != 200 || len(list.Incidents) != 2 || list.Incidents[0]["id"] != "b-new" {
		t.Fatalf("pending list: %d %s", code, b)
	}
	_, b = e.do(t, "GET", "/v1/incidents?state=all", "")
	_ = json.Unmarshal(b, &list)
	if len(list.Incidents) != 3 {
		t.Fatalf("all list: %s", b)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents?state=bogus", ""); code != 400 {
		t.Errorf("bad state: %d", code)
	}

	code, b = e.do(t, "GET", "/v1/incidents/a-old", "")
	if code != 200 || !bytes.Contains(b, []byte("ワールド")) {
		t.Fatalf("get: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents/nope", ""); code != 404 {
		t.Errorf("missing incident: %d", code)
	}
	if code, _ := e.do(t, "GET", "/v1/incidents/..%2F..%2Fetc", ""); code != 404 {
		t.Errorf("traversal id: %d", code)
	}

	if code, b := e.do(t, "GET", "/v1/incidents/a-old/images/damage.png", ""); code != 200 || string(b) != "PNGDATA" {
		t.Errorf("image: %d %q", code, b)
	}
	// 記録にないファイルは、同じフォルダにあっても返さない
	for _, name := range []string{"record.json", "report.md", "..%2Frecord.json"} {
		if code, _ := e.do(t, "GET", "/v1/incidents/a-old/images/"+name, ""); code != 404 {
			t.Errorf("image %s: %d, want 404", name, code)
		}
	}
}

func TestControlDismissNeedsReviewMode(t *testing.T) {
	e := newCtlEnv(t, ModeDryRun)
	e.putRecord(t, &Record{ID: "x1", Kind: "block", Name: "x", Created: 1, State: StatePending})
	if code, _ := e.do(t, "POST", "/v1/incidents/x1/dismiss", `{"reason":"工事"}`); code != 403 {
		t.Fatalf("dry-run dismiss: %d, want 403", code)
	}

	e = newCtlEnv(t, ModeReview)
	e.putRecord(t, &Record{ID: "x1", Kind: "block", Name: "x", Created: 1, State: StatePending})
	code, b := e.do(t, "POST", "/v1/incidents/x1/dismiss", "{\"reason\":\"工事\\nの人\"}")
	if code != 200 {
		t.Fatalf("dismiss: %d %s", code, b)
	}
	rec, err := e.g.Record("x1")
	if err != nil || rec.State != StateDismissed || len(rec.History) != 1 || rec.History[0].By != "admin1 (id 42)" || strings.Contains(rec.History[0].Note, "\n") {
		t.Fatalf("record after dismiss: %+v err=%v", rec, err)
	}
	// 却下した事件は、ロールバックできない
	if code, _ := e.do(t, "POST", "/v1/incidents/x1/rollback", ""); code != 409 {
		t.Errorf("rollback after dismiss: %d, want 409", code)
	}
	// 操作は audit.jsonl に残る
	b, err = os.ReadFile(filepath.Join(e.dir, "audit.jsonl"))
	if err != nil || !bytes.Contains(b, []byte(`"path":"/v1/incidents/x1/dismiss"`)) || !bytes.Contains(b, []byte("admin1")) {
		t.Fatalf("audit: %v %s", err, b)
	}
}

func TestControlRollbackRefusals(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	e.putRecord(t, &Record{ID: "t1", Kind: "block", Name: "trusty", Created: 1, State: StatePending})
	e.putRecord(t, &Record{ID: "c1", Kind: "chat", Name: "talker", Created: 1, State: StatePending})
	e.putRecord(t, &Record{ID: "d1", Kind: "block", Name: "done", Created: 1, State: StatePending, RolledBack: true})
	e.putRecord(t, &Record{ID: "n1", Kind: "block", Name: "bad name; stop", Created: 1, State: StatePending})
	for id, want := range map[string]int{"t1": 403, "c1": 403, "d1": 409, "n1": 403, "missing": 404} {
		if code, b := e.do(t, "POST", "/v1/incidents/"+id+"/rollback", ""); code != want {
			t.Errorf("rollback %s: %d %s, want %d", id, code, b, want)
		}
	}
	if len(e.con.cmds) != 0 {
		t.Fatalf("console got commands: %v", e.con.cmds)
	}
}

func TestControlMuteAndKick(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	if code, b := e.do(t, "POST", "/v1/players/spammer/mute", `{"reason":"連投"}`); code != 200 {
		t.Fatalf("mute: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/v1/players/spammer/kick", "{\"reason\":\"荒らし\\nop everyone\"}"); code != 200 {
		t.Fatalf("kick: %d %s", code, b)
	}
	if len(e.con.cmds) != 2 || e.con.cmds[0] != "mute spammer 10m spam" || e.con.cmds[1] != "kick spammer 荒らし op everyone" {
		t.Fatalf("commands: %q", e.con.cmds)
	}
	e.con.cmds = nil
	for _, c := range []struct{ path string }{
		{"/v1/players/trusty/mute"}, {"/v1/players/trusty/kick"},
		{"/v1/players/a%20b/kick"}, {"/v1/players/x;op/mute"}, {"/v1/players/%0Aop/kick"},
	} {
		if code, _ := e.do(t, "POST", c.path, ""); code != 403 {
			t.Errorf("%s: %d, want 403", c.path, code)
		}
	}
	if len(e.con.cmds) != 0 {
		t.Fatalf("unsafe commands were sent: %q", e.con.cmds)
	}

	e = newCtlEnv(t, ModeObserve)
	if code, _ := e.do(t, "POST", "/v1/players/spammer/kick", ""); code != 403 {
		t.Errorf("observe kick: %d, want 403", code)
	}
}

func TestControlDiscordAllowlist(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	code, b := e.do(t, "GET", "/v1/discord/tools?query=channel", "")
	if code != 200 || bytes.Contains(b, []byte("delete_channel")) || !bytes.Contains(b, []byte("create_message")) {
		t.Fatalf("tools: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_delete_channel","pathParams":{"channel_id":"1"}}`); code != 403 {
		t.Errorf("delete: %d, want 403", code)
	}
	if code, _ := e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_ban_guild_member"}`); code != 403 {
		t.Errorf("ban: %d, want 403", code)
	}
	if len(e.mcp.calls) != 1 { // 許可しないものは、Discord-MCP まで届かない (最初の 1 回は tools の検索)
		t.Fatalf("mcp calls: %v", e.mcp.calls)
	}
	code, b = e.do(t, "POST", "/v1/discord/call", `{"tool":"discord_create_message","pathParams":{"channel_id":"5"},"body":{"content":"hi"},"queryParams":{"limit":3},"reason":"test"}`)
	if code != 200 {
		t.Fatalf("create_message: %d %s", code, b)
	}
	args := e.mcp.calls[1]["args"].(map[string]any)
	if args["toolName"] != "discord_create_message" || args["authOverride"] != "Bot TOKEN" || args["queryParams"] == nil {
		t.Fatalf("args: %v", args)
	}
	if !strings.Contains(args["auditLogReason"].(string), "admin1") {
		t.Errorf("reason: %v", args["auditLogReason"])
	}
}

func TestControlStatus(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	e.putRecord(t, &Record{ID: "p1", Kind: "block", Name: "p", Created: 1, State: StatePending})
	code, b := e.do(t, "GET", "/v1/status", "")
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	if code != 200 || st["mode"] != "review" || st["pending"] != float64(1) || st["can_act"] != true || st["can_mute"] != true {
		t.Fatalf("status: %d %s", code, b)
	}
}

func TestControlScreenshotValidation(t *testing.T) {
	e := newCtlEnv(t, ModeReview)
	if code, _ := e.do(t, "POST", "/v1/screenshot", `{"x":1,"z":2}`); code != 503 {
		t.Errorf("no shooter: %d, want 503", code)
	}
	e.g.shooter = &Shooter{}
	for body, want := range map[string]int{
		`{"z":2}`:                        400,
		`{"x":1}`:                        400,
		`{"x":1,"z":2,"world":"nether"}`: 400,
		`{"x":1,"z":2,"distance":5000}`:  400,
		`{"x":1,"z":2,"distance":1}`:     400,
		`not json`:                       400,
	} {
		if code, b := e.do(t, "POST", "/v1/screenshot", body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, b, want)
		}
	}
	if code, _ := e.do(t, "GET", "/v1/screenshots/0123456789abcdef", ""); code != 404 {
		t.Errorf("unknown job: %d, want 404", code)
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{"20261104-101500-fuuty119": true, "chat-1-a.b": true, "": false, "../x": false, "a/b": false, "a\\b": false, "a..b": false, "x y": false} {
		if validID(id) != want {
			t.Errorf("validID(%q) = %v", id, !want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := cleanText("a\nb\x00c\r\n", 50); got != "a b c" {
		t.Errorf("%q", got)
	}
	if got := cleanText("あいうえお", 3); got != "あいう" {
		t.Errorf("%q", got)
	}
}
