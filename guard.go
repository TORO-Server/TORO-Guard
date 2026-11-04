package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Guard は、CoreProtect の変更を見張り、荒らしを見つけたら、証拠の画像を作って通報し、ロールバックする。
type Guard struct {
	cfg      *Config
	cp       *CP
	det      *Detector
	store    *Store
	ops      *Ops      // コンソール (observe では使わない。replay では nil)
	shooter  *Shooter  // 画像 (observe では使わない)
	reporter *Reporter // live だけ
	warps    []Warp
	log      *slog.Logger

	mu         sync.Mutex
	suppressed map[int]int64 // user → この時刻まで、検知しても無視する (道具を使った人、信頼する人)
	queue      chan *Incident
}

func NewGuard(cfg *Config, cp *CP, st *Store, ops *Ops, sh *Shooter, rep *Reporter, log *slog.Logger) *Guard {
	return &Guard{
		cfg: cfg, cp: cp, det: NewDetector(cfg), store: st, ops: ops, shooter: sh, reporter: rep,
		warps: LoadWarps(cfg.WarpsDir), log: log,
		suppressed: map[int]int64{}, queue: make(chan *Incident, 16),
	}
}

// Run は見張りの本体。ctx が終わるまで回る。
func (g *Guard) Run(ctx context.Context) error {
	if g.store.S.Cursor == 0 {
		n, err := g.cp.MaxRowID(ctx)
		if err != nil {
			return fmt.Errorf("max rowid: %w", err)
		}
		g.mu.Lock()
		g.store.S.Cursor = n
		_ = g.store.Save()
		g.mu.Unlock()
		g.log.Info("起動: 今ある最後の行から見る", "cursor", n)
	}
	// 前回の続きの事件は、またキューに入れる
	g.mu.Lock()
	for _, inc := range g.store.S.Incidents {
		inc.Queued = false
	}
	g.mu.Unlock()

	go g.worker(ctx)

	tick := time.NewTicker(g.cfg.PollInterval)
	defer tick.Stop()
	for {
		if err := g.step(ctx); err != nil && ctx.Err() == nil {
			g.log.Error("見張りの 1 回が失敗", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (g *Guard) step(ctx context.Context) error {
	for {
		events, err := g.cp.Poll(ctx, g.store.S.Cursor, 20000)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		g.mu.Lock()
		for _, e := range events {
			g.store.S.Cursor = e.RowID
			if now-e.Time > int64(g.cfg.MaxEventAge.Seconds()) { // 古い行 (止まっていたあいだのもの) は、見ても報告しない
				continue
			}
			g.onEvent(ctx, e)
		}
		g.mu.Unlock()
		if len(events) < 20000 {
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeIdle(time.Now().Unix())
	return g.store.Save()
}

// onEvent は g.mu を持って呼ぶ。
func (g *Guard) onEvent(ctx context.Context, e Event) {
	key := fmt.Sprint(e.User)
	if inc := g.store.S.Incidents[key]; inc != nil && e.Time > inc.Last {
		inc.Last = e.Time
	}
	t := g.det.Feed(e)
	if t == nil {
		return
	}
	until := g.suppressed[e.User]
	if e.Time < until || g.store.S.Incidents[key] != nil {
		return
	}
	g.consider(ctx, *t, e)
}

// consider は、閾値を超えた人を、事件にするか決める (g.mu を持って呼ぶ)。
func (g *Guard) consider(ctx context.Context, t Trigger, e Event) {
	u, err := g.cp.User(ctx, t.User)
	if err != nil {
		g.log.Error("ユーザーが引けない", "user", t.User, "err", err)
		return
	}
	suppress := func(why string, d time.Duration) {
		g.suppressed[t.User] = e.Time + int64(d.Seconds())
		g.log.Info("検知したが対象外", "player", u.Name, "why", why, "trigger", t.String())
	}
	if strings.HasPrefix(u.Name, "#") { // #tnt, #fire, #decay など (人ではない)
		suppress("not a player", time.Hour)
		return
	}
	if g.cfg.TrustedPlayers[strings.ToLower(u.Name)] {
		suppress("trusted", time.Hour)
		return
	}
	if LoadOps(g.cfg.OpsFile)[strings.ToLower(u.Name)] {
		suppress("op", time.Hour)
		return
	}
	cmd, err := g.cp.RecentEditCommand(ctx, t.User, e.Time-int64(g.cfg.WEGrace.Seconds()))
	if err != nil {
		g.log.Error("コマンドの確認に失敗", "err", err)
	}
	if cmd != "" {
		suppress("edit command: "+cmd, 10*time.Minute)
		return
	}

	inc := &Incident{
		ID:     fmt.Sprintf("%s-%s", time.Unix(e.Time, 0).In(jst).Format("20060102-150405"), u.Name),
		UserID: t.User, Name: u.Name, UUID: u.UUID,
		Start: e.Time - int64(t.Window), Last: e.Time, Trigger: t.String(),
	}
	g.store.S.Incidents[fmt.Sprint(t.User)] = inc
	g.log.Warn("荒らしを検知", "player", u.Name, "uuid", u.UUID, "trigger", t.String(), "incident", inc.ID, "mode", g.cfg.Mode)

	if g.cfg.Mode == ModeLive && g.cfg.KickEnabled && g.ops != nil {
		if err := g.ops.Kick(ctx, u.Name, "ブロックを不自然な速さで変えたため"); err != nil {
			g.log.Error("キックに失敗", "err", err)
		} else {
			g.log.Warn("キックした", "player", u.Name)
		}
	}
}

// closeIdle は、動きが止まった (または長すぎる) 事件を閉じて、報告のキューに入れる (g.mu を持って呼ぶ)。
func (g *Guard) closeIdle(now int64) {
	for _, inc := range g.store.S.Incidents {
		if inc.Queued {
			continue
		}
		quiet := now-inc.Last >= int64(g.cfg.Quiet.Seconds())
		long := now-inc.Start >= int64(g.cfg.MaxIncident.Seconds())
		if !quiet && !long {
			continue
		}
		select {
		case g.queue <- inc:
			inc.Queued = true
		default:
		}
	}
}

// worker は閉じた事件を 1 つずつ処理する (コンソールとブラウザを同時に使わないため)。
func (g *Guard) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case inc := <-g.queue:
			// 閉じる前に Reset (同じ人の続きは新しい事件として数え直す)
			g.mu.Lock()
			g.det.Reset(inc.UserID)
			delete(g.store.S.Incidents, fmt.Sprint(inc.UserID))
			_ = g.store.Save()
			g.mu.Unlock()
			if res, err := g.Process(ctx, inc); err != nil {
				g.log.Error("事件の処理に失敗", "incident", inc.ID, "err", err)
			} else {
				g.log.Info("事件を処理した", "incident", inc.ID, "dir", res.Dir, "posted", res.ThreadID != "", "rolled_back", res.Out.RolledBack)
			}
		}
	}
}

// Result は 1 件の事件の処理の結果。
type Result struct {
	Dir      string
	Stats    *Stats
	Report   string
	Out      Outcome
	ThreadID string
	Images   []ReportFile
}

// Process は 1 件の事件を、mode に合わせて処理する。
//
//	observe: まとめを記録するだけ
//	dry-run: 画像と報告文を作ってフォルダに置くだけ (投稿もロールバックもしない)
//	live:    画像 → ロールバック → 復旧後の画像 → 通報
func (g *Guard) Process(ctx context.Context, inc *Incident) (*Result, error) {
	return g.processWindow(ctx, inc, time.Now().Unix())
}

// processWindow は [inc.Start, end] の分を処理する (replay は、過去の end を渡す)。
func (g *Guard) processWindow(ctx context.Context, inc *Incident, end int64) (*Result, error) {
	user := UserInfo{ID: inc.UserID, Name: inc.Name, UUID: inc.UUID}
	now := time.Now().Unix()
	stats, err := g.cp.Stats(ctx, user, inc.Start-2, end)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	if stats.Empty() {
		return nil, fmt.Errorf("no block changes found for %s", inc.Name)
	}
	dir := filepath.Join(g.cfg.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Dir: dir, Stats: stats}
	mapID := g.cfg.MapIDs[stats.World]

	// 1. 証拠の絵 (BlueMap が使えなくても作れる、真上からの図)
	heat := filepath.Join(dir, "heatmap.png")
	if err := RenderHeatmap(stats, heat); err != nil {
		g.log.Error("真上の図が作れない", "err", err)
		heat = ""
	}

	if g.cfg.Mode == ModeObserve {
		g.log.Info("観察のみ", "incident", inc.ID, "breaks", stats.Breaks, "places", stats.Places, "peak_per_s", stats.PeakPerS)
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": summaryOf(stats)})
	}

	// 2. 被害の BlueMap の画面
	cam := CamFor(stats)
	damage := g.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "damage.png"))

	// 3. ロールバック (live だけ)
	out := Outcome{}
	seconds := int(now-inc.Start) + 5
	out.Cmd = RollbackCmd(inc.Name, seconds, stats.World)
	out.RestoreCmd = RestoreCmd(inc.Name, seconds, stats.World)
	restored := ""
	switch {
	case g.cfg.Mode == ModeLive && g.ops != nil:
		lines, err := g.ops.Rollback(ctx, inc.Name, seconds, stats.World)
		out.Lines = lines
		done, total, derr := g.cp.RolledBack(ctx, inc.UserID, inc.Start-2, end)
		if derr == nil {
			out.RolledDone, out.RolledTotal = done, total
		}
		// コンソールの返事が取れなくても、データベースで「ほぼ全部ロールバックされた」なら成功とする
		if err == nil || (derr == nil && total > 0 && done*100/total >= 95) {
			out.RolledBack = true
		} else if err != nil {
			out.Error = err.Error()
		}
		if out.RolledBack {
			restored = g.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "restored.png"))
		}
	default:
		out.Planned = true
	}
	res.Out = out

	// 4. 報告文と画像
	var files []ReportFile
	var desc []string
	if damage != "" {
		files = append(files, ReportFile{Name: "damage.png", Path: damage})
		desc = append(desc, "1枚目 被害 (BlueMap)")
	}
	if restored != "" {
		files = append(files, ReportFile{Name: "restored.png", Path: restored})
		desc = append(desc, fmt.Sprintf("%d枚目 ロールバック後 (BlueMap)", len(files)))
	}
	if len(files) < 2 && heat != "" {
		files = append(files, ReportFile{Name: "heatmap.png", Path: heat})
		desc = append(desc, fmt.Sprintf("%d枚目 壊した場所 (真上から)", len(files)))
	}
	res.Images = files

	warp := ""
	if w, d, ok := Nearest(g.warps, stats.World, float64(stats.X0+stats.X1)/2, float64(stats.Z0+stats.Z1)/2); ok {
		warp = WarpText(w, d)
	}
	res.Report = BuildReport(ReportInput{Stats: stats, Trigger: inc.Trigger, Warp: warp, Out: out, Images: desc, Mode: g.cfg.Mode, Center: &cam})
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": summaryOf(stats), "outcome": out})

	// 5. 通報 (live だけ)
	if g.cfg.Mode == ModeLive && g.reporter != nil {
		key := threadKey(inc.Name)
		existing := ""
		g.mu.Lock()
		if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		g.mu.Unlock()
		id, err := g.reporter.Post(ctx, existing, ThreadTitle(stats), res.Report, files, out.RolledBack)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		g.mu.Lock()
		g.store.S.Threads[key] = ThreadRef{ThreadID: id, At: now}
		_ = g.store.Save()
		g.mu.Unlock()
	}
	return res, nil
}

// bluemapShot は、書き出し → BlueMap の更新 → 待つ → 撮る。失敗しても事件の処理は続ける (空の文字列を返す)。
func (g *Guard) bluemapShot(ctx context.Context, mapID string, s *Stats, cam Cam, out string) string {
	if g.shooter == nil || mapID == "" {
		return ""
	}
	if g.ops != nil {
		cx, cz := (s.X0+s.X1)/2, (s.Z0+s.Z1)/2
		radius := maxInt(maxInt(s.X1-s.X0, s.Z1-s.Z0)/2+48, 64)
		if err := g.ops.SaveFlush(ctx); err != nil {
			g.log.Warn("ワールドの書き出しに失敗", "err", err)
		}
		if err := g.ops.BlueMapUpdate(ctx, mapID, cx, cz, radius); err != nil {
			g.log.Warn("BlueMap の更新を頼めない", "err", err)
		} else if err := g.ops.WaitBlueMap(ctx, mapID, 6*time.Minute); err != nil {
			g.log.Warn("BlueMap の更新を待ちきれない (古い地図で撮る)", "err", err)
		}
	}
	if err := g.shooter.Shot(ctx, mapID, cam, out); err != nil {
		g.log.Warn("BlueMap の画面が撮れない", "err", err)
		return ""
	}
	return out
}

func summaryOf(s *Stats) map[string]any {
	return map[string]any{
		"player": s.User.Name, "uuid": s.User.UUID, "world": s.World,
		"from": s.From, "to": s.To, "breaks": s.Breaks, "places": s.Places, "clicks": s.Clicks,
		"peak_per_s": s.PeakPerS, "peak_at": s.PeakAt,
		"x": []int{s.X0, s.X1}, "y": []int{s.Y0, s.Y1}, "z": []int{s.Z0, s.Z1},
		"top_breaks": s.TopBreaks, "commands": s.Commands, "containers": s.Containers,
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
