package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func testCfg() *Config {
	return &Config{
		BurstWindow: 5 * time.Second, BreakBurst: 100, PlaceBurst: 150,
		SustainedWindow: 10 * time.Minute, SustainedBreaks: 1500,
	}
}

func feedN(d *Detector, user int, sec int64, action, n int) *Trigger {
	var last *Trigger
	for i := 0; i < n; i++ {
		if t := d.Feed(Event{User: user, Time: sec, Action: action}); t != nil {
			last = t
		}
	}
	return last
}

func TestDetectorHumanSpeedNotFlagged(t *testing.T) {
	d := NewDetector(testCfg())
	// 1 秒に 8 個を 2 分続けても (960 個)、バーストにも長い窓にも当たらない
	for s := int64(1000); s < 1120; s++ {
		if tr := feedN(d, 1, s, actBreak, 8); tr != nil {
			t.Fatalf("flagged at %d: %v", s, tr)
		}
	}
}

func TestDetectorBurstBreak(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actBreak, 60); tr != nil {
		t.Fatalf("early: %v", tr)
	}
	tr := feedN(d, 1, 1002, actBreak, 60) // 5 秒の窓に 120 個
	if tr == nil || tr.Kind != "burst-break" || tr.Breaks < 100 {
		t.Fatalf("want burst-break, got %v", tr)
	}
}

func TestDetectorWindowExpires(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 90)
	// 5 秒以上あいた次の 90 個は、足されない
	if tr := feedN(d, 1, 1010, actBreak, 90); tr != nil {
		t.Fatalf("flagged across the gap: %v", tr)
	}
}

func TestDetectorBurstPlace(t *testing.T) {
	d := NewDetector(testCfg())
	tr := feedN(d, 2, 1000, actPlace, 150)
	if tr == nil || tr.Kind != "burst-place" {
		t.Fatalf("want burst-place, got %v", tr)
	}
}

func TestDetectorSustained(t *testing.T) {
	d := NewDetector(testCfg())
	var tr *Trigger
	// 1 秒に 5 個 (人でも出せる速さ) を 5 分以上続ける → 1500 個で長い窓に当たる
	for s := int64(1000); s < 1400 && tr == nil; s++ {
		tr = feedN(d, 3, s, actBreak, 5)
	}
	if tr == nil || tr.Kind != "sustained-break" {
		t.Fatalf("want sustained-break, got %v", tr)
	}
}

func TestDetectorIgnoresClicksAndSeparatesUsers(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actClick, 500); tr != nil {
		t.Fatalf("click flagged: %v", tr)
	}
	feedN(d, 1, 1000, actBreak, 60)
	if tr := feedN(d, 2, 1000, actBreak, 60); tr != nil {
		t.Fatalf("users mixed: %v", tr)
	}
}

func TestDetectorOutOfOrder(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1005, actBreak, 60)
	// 少し前の秒が後から来ても、窓の中なら足される
	tr := feedN(d, 1, 1003, actBreak, 60)
	if tr == nil {
		t.Fatal("out-of-order rows inside the window must count")
	}
}

func TestDetectorReset(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 99)
	d.Reset(1)
	if tr := feedN(d, 1, 1000, actBreak, 5); tr != nil {
		t.Fatalf("not reset: %v", tr)
	}
}

func TestRollbackCmd(t *testing.T) {
	if got := RollbackCmd("fuuty119", 415, "main"); got != "co rollback u:fuuty119 t:415s r:#main" {
		t.Fatal(got)
	}
	if got := RestoreCmd("fuuty119", 415, "main"); got != "co restore u:fuuty119 t:415s r:#main" {
		t.Fatal(got)
	}
}

func TestParseBlueMapIdle(t *testing.T) {
	idle := []string{"BlueMap Status >", "? 2 render-threads are idle", "? last active 2 hours ago", "? map overworld is updated"}
	if !parseBlueMapIdle(idle, "overworld") {
		t.Fatal("idle+updated must be true")
	}
	busy := []string{"BlueMap Status >", "? 2 render-threads are running", "? map overworld is updated"}
	if parseBlueMapIdle(busy, "overworld") {
		t.Fatal("running must be false")
	}
	notUpdated := []string{"? 2 render-threads are idle", "? map overworld is not updated"}
	if parseBlueMapIdle(notUpdated, "overworld") {
		t.Fatal("not updated must be false")
	}
}

func TestCleanLine(t *testing.T) {
	in := "\x1b[?1l\x1b>\x1b[K[12:35:53 INFO]: CoreProtect - Rollback completed for \"x\".\r"
	got := cleanLine(in)
	if !strings.HasPrefix(got, "[12:35:53 INFO]:") || !rollbackOK.MatchString(got) {
		t.Fatalf("got %q", got)
	}
}

// runOnStream: コマンドを送り、「完了」の行で止まる。送った行は \r で終わる。
func TestRunOnStream(t *testing.T) {
	pr, pw := io.Pipe() // サーバー → こちら
	var sent strings.Builder
	w := writerFunc(func(p []byte) (int, error) {
		sent.Write(p)
		go func() {
			io.WriteString(pw, "\x1b[K[10:00:00 INFO]: noise\r\n")
			io.WriteString(pw, "[10:00:01 INFO]: CoreProtect - Rollback completed for \"x\".\r\n")
		}()
		return len(p), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := runOnStream(ctx, w, pr, "co rollback u:x t:10s r:#main", 3*time.Second, func(l string) bool { return rollbackOK.MatchString(l) })
	if err != nil {
		t.Fatal(err)
	}
	if sent.String() != "co rollback u:x t:10s r:#main\r" {
		t.Fatalf("sent %q", sent.String())
	}
	if len(lines) != 2 || !rollbackOK.MatchString(lines[1]) {
		t.Fatalf("lines %q", lines)
	}
}

func TestRunOnStreamTimeout(t *testing.T) {
	pr, _ := io.Pipe()
	w := writerFunc(func(p []byte) (int, error) { return len(p), nil })
	_, err := runOnStream(context.Background(), w, pr, "x", 300*time.Millisecond, func(string) bool { return false })
	if err == nil {
		t.Fatal("want timeout error")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// fakeConsole は Ops の試験用。
type fakeConsole struct {
	cmds  []string
	reply map[string][]string
}

func (f *fakeConsole) Run(_ context.Context, cmd string, _ time.Duration, done func(string) bool) ([]string, error) {
	f.cmds = append(f.cmds, cmd)
	for prefix, lines := range f.reply {
		if strings.HasPrefix(cmd, prefix) {
			return lines, nil
		}
	}
	return nil, nil
}

func TestOpsRollbackFailureDetected(t *testing.T) {
	// 返事が「失敗」の行なら、エラーにする (done の判定が NG の行で true を返す)
	c := &fakeConsole{}
	o := Ops{C: &failConsole{c}}
	if _, err := o.Rollback(context.Background(), "x", 10, "main"); err == nil {
		t.Fatal("want error")
	}
}

type failConsole struct{ *fakeConsole }

func (f *failConsole) Run(ctx context.Context, cmd string, to time.Duration, done func(string) bool) ([]string, error) {
	line := "[10:00:00 INFO]: CoreProtect - Rollback failed."
	done(line)
	return []string{line}, nil
}

func TestReportText(t *testing.T) {
	s := &Stats{
		User: UserInfo{Name: "fuuty119", UUID: "53b4dddf-b884-4439-b26b-be0741d26e8c"}, World: "main",
		From: 1793757149, To: 1793757495, Breaks: 2642, Places: 158, PeakPerS: 421,
		X0: -5712, X1: -5612, Y0: 55, Y1: 94, Z0: -10204, Z1: -10049,
		TopBreaks: []MatCount{{"stone", 820}, {"sand", 357}},
		Commands:  []string{"/co i", "/co i"},
	}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst-break", Warp: "ワープ kanso_town の近く (約 12 ブロック)",
		Center: &Cam{X: -5650, Z: -10125}, Out: Outcome{RolledBack: true, RolledDone: 2800, RolledTotal: 2800, RestoreCmd: "co restore u:fuuty119 t:415s r:#main"}, Mode: ModeLive,
		Images: []string{"1枚目 被害"}})
	for _, want := range []string{"**ワールド**: main", "**座標**: -5650 -10125 付近", "fuuty119", "2,642", "421 個/秒", "stone 820", "`/co i` ×2", "ロールバック済み (2,800 / 2,800 件)", "co restore u:fuuty119", "kanso_town"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if len([]rune(txt)) > 2000 {
		t.Fatal("over discord limit")
	}
	dry := BuildReport(ReportInput{Stats: s, Out: Outcome{Planned: true, Cmd: "co rollback u:fuuty119 t:415s r:#main"}, Mode: ModeDryRun})
	if !strings.Contains(dry, "試運転") || !strings.Contains(dry, "対応 (予定)") {
		t.Fatalf("dry-run text: %s", dry)
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 2642: "2,642", 1234567: "1,234,567"} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d)=%s want %s", in, got, want)
		}
	}
}

func TestCamFor(t *testing.T) {
	// 点が 1 つの塊 (x -5660..-5640) と、遠くの外れ値 1 個 → 中心は塊、距離は外れ値で伸びない
	s := &Stats{X0: -5712, X1: -5602, Y0: 55, Y1: 94, Z0: -10210, Z1: -10048}
	for x := -5660; x <= -5640; x++ {
		for z := -10130; z <= -10120; z++ {
			s.Columns = append(s.Columns, Column{X: x, Z: z, N: 10})
		}
	}
	s.Columns = append(s.Columns, Column{X: -5712, Z: -10048, N: 1})
	c := CamFor(s)
	if c.X < -5660 || c.X > -5640 || c.Z < -10130 || c.Z > -10120 {
		t.Fatalf("center %v", c)
	}
	if c.Dist < 70 || c.Dist > 150 {
		t.Fatalf("outlier stretched the camera: dist %v", c.Dist)
	}
	if h := c.Hash("overworld"); !strings.HasPrefix(h, "#overworld:") || !strings.HasSuffix(h, ":0:0:perspective") {
		t.Fatalf("hash %s", h)
	}
	// 列が無いときは範囲の真ん中
	c2 := CamFor(&Stats{X0: 0, X1: 100, Z0: 0, Z1: 200, Y0: 64, Y1: 64})
	if c2.X != 50 || c2.Z != 100 {
		t.Fatalf("bbox center %v", c2)
	}
}
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.S.Cursor = 42
	st.S.Incidents["7"] = &Incident{ID: "a", UserID: 7, Name: "x", Start: 1, Last: 2}
	st.S.Threads["x"] = ThreadRef{ThreadID: "99", At: 5}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.S.Cursor != 42 || st2.S.Incidents["7"].Name != "x" || st2.S.Threads["x"].ThreadID != "99" {
		t.Fatalf("%+v", st2.S)
	}
}

func TestConfigMode(t *testing.T) {
	t.Setenv("GUARD_MODE", "nope")
	if _, err := loadConfig(); err == nil {
		t.Fatal("bad mode must fail")
	}
	t.Setenv("GUARD_MODE", "")
	c, err := loadConfig()
	if err != nil || c.Mode != ModeObserve {
		t.Fatalf("default must be observe: %v %v", c, err)
	}
}

func TestLoadBotToken(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.yml"
	if err := writeFile(p, "ConfigVersion: 1\nBotToken: \"abc.def.ghi\"\nChannels: {\"global\": \"1\"}\n"); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadBotToken(p)
	if err != nil || tok != "abc.def.ghi" {
		t.Fatalf("%q %v", tok, err)
	}
	if err := writeFile(p, "BotToken: \"\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotToken(p); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestNearestWarp(t *testing.T) {
	ws := []Warp{{Name: "spawn", World: "main", X: 0, Z: 0}, {Name: "kanso_town", World: "main", X: -5728, Z: -10111}, {Name: "far", World: "flat", X: -5700, Z: -10100}}
	w, d, ok := Nearest(ws, "main", -5662, -10126)
	if !ok || w.Name != "kanso_town" || d > 100 {
		t.Fatalf("%v %v %v", w, d, ok)
	}
	if WarpText(w, d) == "" {
		t.Fatal("near warp must have text")
	}
	if WarpText(Warp{Name: "x"}, 2000) != "" {
		t.Fatal("far warp must be omitted")
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func TestLoadOps(t *testing.T) {
	p := t.TempDir() + "/ops.json"
	if err := writeFile(p, `[{"uuid":"1","name":"MarumaSan","level":4},{"uuid":"2","name":"Other","level":2}]`); err != nil {
		t.Fatal(err)
	}
	ops := LoadOps(p)
	if !ops["marumasan"] || !ops["other"] || len(ops) != 2 {
		t.Fatalf("%v", ops)
	}
	if len(LoadOps("")) != 0 || len(LoadOps(t.TempDir()+"/none.json")) != 0 {
		t.Fatal("missing file must be empty")
	}
}
