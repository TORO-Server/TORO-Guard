package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 窓口 (control) から頼まれる操作。人が (または AI が) 確かめて決めたことを、ここで実行する。
// どれも、動かし方が review か live のときだけ受ける。by は、頼んだ人 (記録に残す)。

var (
	// ErrForbidden は、してはいけない相手 (信頼する人・op) や、受け付けない動かし方のとき。
	ErrForbidden = errors.New("forbidden")
	// ErrConflict は、すでにやった・いま別の操作が動いているなど、いまはできないとき。
	ErrConflict = errors.New("conflict")
	// ErrUnavailable は、必要な部品 (コンソール・Discord・ブラウザ) が使えない設定のとき。
	ErrUnavailable = errors.New("unavailable")
)

func (g *Guard) checkMode() error {
	if !g.cfg.Mode.CanAct() {
		return fmt.Errorf("%w: GUARD_MODE=%s のあいだは、操作を受け付けません (review か live にしてください)", ErrForbidden, g.cfg.Mode)
	}
	return nil
}

// protected は、荒らしとして扱ってはいけない人 (信頼する人・op)。
func (g *Guard) protected(name string) bool {
	l := strings.ToLower(name)
	return g.cfg.TrustedPlayers[l] || LoadOps(g.cfg.OpsFile)[l]
}

var ctlRE = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// cleanText は、コンソールや Discord に渡す理由の文字から、制御文字 (改行など) を除いて短くする。
func cleanText(s string, max int) string {
	s = strings.TrimSpace(ctlRE.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// Dismiss は、荒らしではないと判断した事件を閉じる。
func (g *Guard) Dismiss(id, by, reason string) (*Record, error) {
	rec, err := g.update(id, func(r *Record) {
		if r.State == StatePending {
			r.State = StateDismissed
			r.decide(time.Now().Unix(), by, "dismiss", cleanText(reason, 300))
		}
	})
	return rec, err
}

// ReportIncident は、事件をフォーラムに通報する。note があれば、本文の頭に足す。
// ブロックの荒らしは、いまのロールバックの状態で、報告文を作り直す。
func (g *Guard) ReportIncident(ctx context.Context, id, by, note string) (*Record, error) {
	if err := g.checkMode(); err != nil {
		return nil, err
	}
	if g.reporter == nil {
		return nil, fmt.Errorf("%w: Discord に投稿する設定 (DiscordSRV のトークンと Discord-MCP) がありません", ErrUnavailable)
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	rec, err := g.Record(id)
	if err != nil {
		return nil, err
	}
	if rec.Reported {
		return nil, fmt.Errorf("%w: すでに通報しています (投稿 %s)", ErrConflict, rec.Thread)
	}
	if rec.Busy != "" {
		return nil, fmt.Errorf("%w: いま %s を実行中です。終わってから通報してください", ErrConflict, rec.Busy)
	}
	dir := g.reportDir(id)
	var files []ReportFile
	for _, n := range rec.Images {
		files = append(files, ReportFile{Name: n, Path: filepath.Join(dir, filepath.Base(n))})
	}

	var text string
	if rec.Kind == "block" {
		user := UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
		stats, err := g.cp.Stats(ctx, user, rec.Start-2, rec.End)
		if err != nil {
			return nil, fmt.Errorf("stats: %w", err)
		}
		out := Outcome{Cmd: rec.RollbackCmd, RestoreCmd: rec.RestoreCmd, RolledBack: rec.RolledBack, AI: rec.AI, Planned: !rec.RolledBack}
		if rec.RolledBack {
			done, total, derr := g.cp.RolledBack(ctx, rec.UserID, rec.Start-2, rec.End)
			if derr == nil {
				out.RolledDone, out.RolledTotal = done, total
			}
		}
		text = g.blockReport(stats, rec.Trigger, out, files, CamFor(stats))
	} else {
		b, err := os.ReadFile(filepath.Join(dir, "report.md"))
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	if n := cleanText(note, 500); n != "" {
		text = "**確認した人のメモ**: " + strings.ReplaceAll(n, "@", "＠") + "\n" + text
	}
	if r := []rune(text); len(r) > 1900 {
		text = string(r[:1900])
	}

	now := time.Now().Unix()
	key := threadKey(rec.Name)
	existing := ""
	g.mu.Lock()
	if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
		existing = t.ThreadID
	}
	g.mu.Unlock()
	var tags []string
	if rec.RolledBack && g.reporter.tagID != "" {
		tags = append(tags, g.reporter.tagID)
	}
	if rec.Muted && g.cfg.ChatMuteTagID != "" {
		tags = append(tags, g.cfg.ChatMuteTagID)
	}
	title := rec.Name
	thread, err := g.reporter.PostTags(ctx, existing, title, text, files, tags)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	g.mu.Lock()
	g.store.S.Threads[key] = ThreadRef{ThreadID: thread, At: now}
	_ = g.store.Save()
	g.mu.Unlock()
	return g.update(id, func(r *Record) {
		r.State, r.Reported, r.Thread = StateReported, true, thread
		r.decide(now, by, "report", cleanText(note, 300))
	})
}

// StartRollback は、ブロックの荒らしのロールバックを裏で始める (数分かかることがある)。
// 終わったかは、事件の記録 (busy が消え、rolled_back が立つ) で分かる。
func (g *Guard) StartRollback(ctx context.Context, id, by string) (*Record, error) {
	if err := g.checkMode(); err != nil {
		return nil, err
	}
	if g.ops == nil {
		return nil, fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	}
	rec, err := g.Record(id)
	if err != nil {
		return nil, err
	}
	switch {
	case rec.Kind != "block":
		return nil, fmt.Errorf("%w: チャットの事件はロールバックできません", ErrForbidden)
	case rec.RolledBack:
		return nil, fmt.Errorf("%w: すでにロールバックしています (やり直すときは、運営がコンソールで %s)", ErrConflict, rec.RestoreCmd)
	case rec.State == StateDismissed:
		return nil, fmt.Errorf("%w: 却下した事件です", ErrConflict)
	case !validName(rec.Name):
		return nil, fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません。運営が手で行ってください (%s)", ErrForbidden, rec.Name, rec.RollbackCmd)
	case g.protected(rec.Name):
		return nil, fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, rec.Name)
	}
	g.mu.Lock()
	if b := g.busy[id]; b != "" {
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: いま %s を実行中です", ErrConflict, b)
	}
	g.busy[id] = "rollback"
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.busy, id)
			g.mu.Unlock()
		}()
		g.opMu.Lock()
		defer g.opMu.Unlock()
		note, err := g.runRollback(ctx, rec)
		if err != nil {
			g.log.Error("ロールバックに失敗", "incident", id, "err", err)
			note = "失敗: " + err.Error()
		}
		_, _ = g.update(id, func(r *Record) {
			if err == nil {
				r.RolledBack = true
			}
			r.decide(time.Now().Unix(), by, "rollback", note)
		})
	}()
	rec.Busy = "rollback"
	return rec, nil
}

// runRollback はロールバックを実行し、前の画面と同じカメラで「ロールバック後」の画像を撮る。
// ロールバックそのものが成功したとき (または、データベースでほぼ全部が戻ったとき) は、エラーを返さない。
func (g *Guard) runRollback(ctx context.Context, rec *Record) (string, error) {
	seconds := int(time.Now().Unix()-rec.Start) + 5
	lines, err := g.ops.Rollback(ctx, rec.Name, seconds, rec.World)
	done, total, derr := g.cp.RolledBack(ctx, rec.UserID, rec.Start-2, rec.End)
	ok := err == nil || (derr == nil && total > 0 && done*100/total >= 95)
	if !ok {
		return "", err
	}
	note := fmt.Sprintf("%d/%d", done, total)
	if derr != nil {
		note = strings.Join(tail(lines, 2), " / ")
	}

	user := UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
	stats, serr := g.cp.Stats(ctx, user, rec.Start-2, rec.End)
	if serr != nil || stats.Empty() || g.shooter == nil || g.cfg.MapIDs[stats.World] == "" {
		return note, nil
	}
	out := filepath.Join(g.reportDir(rec.ID), "restored.png")
	got := g.bluemapShot(ctx, g.cfg.MapIDs[stats.World], stats, CamFor(stats), out,
		AnnotFor(stats, fmt.Sprintf("%s  RESTORED (rollback done)", stats.User.Name), "yellow = affected area", false), true)
	if got != "" {
		_, _ = g.update(rec.ID, func(r *Record) {
			for _, n := range r.Images {
				if n == "restored.png" {
					return
				}
			}
			r.Images = append(r.Images, "restored.png")
		})
	}
	return note, nil
}

// Mute は、ミュートのコマンド (CHAT_MUTE_CMD) を送る。
func (g *Guard) Mute(ctx context.Context, name, by, reason string) (string, error) {
	if err := g.checkMode(); err != nil {
		return "", err
	}
	cmd := MuteCommand(g.cfg.ChatMuteCmd, name)
	switch {
	case g.ops == nil:
		return "", fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	case strings.TrimSpace(g.cfg.ChatMuteCmd) == "":
		return "", fmt.Errorf("%w: CHAT_MUTE_CMD が空なので、ミュートできません", ErrUnavailable)
	case cmd == "":
		return "", fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", ErrForbidden, name)
	case g.protected(name):
		return "", fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, name)
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	lines, err := g.ops.C.Run(ctx, cmd, 5*time.Second, nil)
	if err != nil {
		return "", err
	}
	g.log.Warn("ミュートした", "player", name, "by", by, "reason", cleanText(reason, 100))
	return strings.Join(tail(lines, 2), " / "), nil
}

// Kick は、プレイヤーを理由つきで退出させる。
func (g *Guard) Kick(ctx context.Context, name, by, reason string) (string, error) {
	if err := g.checkMode(); err != nil {
		return "", err
	}
	switch {
	case g.ops == nil:
		return "", fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	case !validName(name):
		return "", fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", ErrForbidden, name)
	case g.protected(name):
		return "", fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, name)
	}
	reason = cleanText(reason, 100)
	if reason == "" {
		reason = "運営の判断による"
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	if err := g.ops.Kick(ctx, name, reason); err != nil {
		return "", err
	}
	g.log.Warn("キックした", "player", name, "by", by, "reason", reason)
	return "kicked", nil
}
