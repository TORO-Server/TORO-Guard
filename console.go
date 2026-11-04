package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Console は Minecraft のサーバーのコンソール。
// toro-main には RCON も shell も無いので、本番では Docker の attach (docker attach と同じ) で行を送る。
type Console interface {
	// Run は cmd を 1 行送り、出力の行を done が true を返すまで (または timeout まで) 集めて返す。
	// done が nil なら、timeout まで集める。
	Run(ctx context.Context, cmd string, timeout time.Duration, done func(line string) bool) ([]string, error)
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[=>]`)

func cleanLine(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.TrimSpace(s)
}

// DockerConsole は Docker Engine の API (unix ソケット) で、コンテナの標準入出力に attach する。
type DockerConsole struct {
	Socket    string
	Container string
	mu        sync.Mutex
}

func (d *DockerConsole) Run(ctx context.Context, cmd string, timeout time.Duration, done func(string) bool) ([]string, error) {
	// 1 度に 1 つのコマンドだけ (出力が混ざらないように)
	d.mu.Lock()
	defer d.mu.Unlock()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.Socket)
	if err != nil {
		return nil, fmt.Errorf("docker socket: %w", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("POST /containers/%s/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nUpgrade: tcp\r\nConnection: Upgrade\r\nContent-Length: 0\r\n\r\n", d.Container)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("docker attach: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker attach: HTTP %d", resp.StatusCode)
	}
	var stream io.Reader = br
	if strings.Contains(resp.Header.Get("Content-Type"), "multiplexed") {
		stream = &demux{r: br}
	}
	return runOnStream(ctx, conn, stream, cmd, timeout, done)
}

// runOnStream は、つながった入出力に cmd を送り、出た行を集める (試験でも使う)。
func runOnStream(ctx context.Context, w io.Writer, r io.Reader, cmd string, timeout time.Duration, done func(string) bool) ([]string, error) {
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if l := cleanLine(sc.Text()); l != "" {
				select {
				case lines <- l:
				default: // 読む側が遅いときは捨てる
				}
			}
		}
	}()

	// 端末の描き直しが落ち着くのを少し待つ
	settle := time.NewTimer(400 * time.Millisecond)
	defer settle.Stop()
drain:
	for {
		select {
		case <-lines:
		case <-settle.C:
			break drain
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if _, err := io.WriteString(w, cmd+"\r"); err != nil {
		return nil, err
	}
	var out []string
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return out, nil
			}
			out = append(out, l)
			if done != nil && done(l) {
				return out, nil
			}
		case <-deadline.C:
			if done != nil {
				return out, fmt.Errorf("timeout waiting for output of %q", cmd)
			}
			return out, nil
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}

// demux は tty でないコンテナの、多重化された出力 (8 バイトの頭 + 本体) を 1 本にほどく。
type demux struct {
	r    io.Reader
	left int
}

func (d *demux) Read(p []byte) (int, error) {
	for d.left == 0 {
		var h [8]byte
		if _, err := io.ReadFull(d.r, h[:]); err != nil {
			return 0, err
		}
		d.left = int(binary.BigEndian.Uint32(h[4:]))
	}
	if len(p) > d.left {
		p = p[:d.left]
	}
	n, err := d.r.Read(p)
	d.left -= n
	return n, err
}

// ---- よく使うコマンド ----

// Ops はサーバーに送るコマンドの組み立てと、返事の読み取り。
type Ops struct{ C Console }

var (
	savedRE    = regexp.MustCompile(`(?i)saved the (game|world)|flushed`)
	rollbackOK = regexp.MustCompile(`(?i)rollback completed`)
	rollbackNG = regexp.MustCompile(`(?i)rollback (failed|cancel+ed)|coreprotect.*no data found`)
)

// SaveFlush はワールドをディスクに書き出す (BlueMap はディスクの region ファイルを読むので)。
func (o Ops) SaveFlush(ctx context.Context) error {
	_, err := o.C.Run(ctx, "save-all flush", 90*time.Second, func(l string) bool { return savedRE.MatchString(l) })
	return err
}

// BlueMapUpdate は範囲を描き直す仕事を作る。
func (o Ops) BlueMapUpdate(ctx context.Context, mapID string, x, z, radius int) error {
	_, err := o.C.Run(ctx, fmt.Sprintf("bluemap update %s %d %d %d", mapID, x, z, radius), 8*time.Second, nil)
	return err
}

// BlueMapIdle は BlueMap が仕事を終えて、地図が最新になっているか。
func (o Ops) BlueMapIdle(ctx context.Context, mapID string) (bool, error) {
	lines, err := o.C.Run(ctx, "bluemap", 4*time.Second, nil)
	if err != nil {
		return false, err
	}
	return parseBlueMapIdle(lines, mapID), nil
}

func parseBlueMapIdle(lines []string, mapID string) bool {
	idle, updated := false, false
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "render-threads are idle") {
			idle = true
		}
		if strings.Contains(ll, "map "+strings.ToLower(mapID)+" is updated") {
			updated = true
		}
	}
	return idle && updated
}

// WaitBlueMap は BlueMap が描き終わるまで待つ。2 回続けて「終わっている」を見たら終わりとする。
func (o Ops) WaitBlueMap(ctx context.Context, mapID string, max time.Duration) error {
	end := time.Now().Add(max)
	okRuns := 0
	for time.Now().Before(end) {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		idle, err := o.BlueMapIdle(ctx, mapID)
		if err != nil {
			return err
		}
		if idle {
			okRuns++
			if okRuns >= 2 {
				return nil
			}
		} else {
			okRuns = 0
		}
	}
	return fmt.Errorf("bluemap did not finish within %s", max)
}

// RollbackCmd は CoreProtect のロールバックのコマンド。時間は「いまからさかのぼる秒」。
func RollbackCmd(player string, seconds int, world string) string {
	return fmt.Sprintf("co rollback u:%s t:%ds r:#%s", player, seconds, world)
}

// RestoreCmd は、まちがえたときに戻すためのコマンド (報告に書く)。
func RestoreCmd(player string, seconds int, world string) string {
	return fmt.Sprintf("co restore u:%s t:%ds r:#%s", player, seconds, world)
}

// Rollback はコマンドを送り、「完了」の行まで待つ。返した行は報告に使う。
func (o Ops) Rollback(ctx context.Context, player string, seconds int, world string) ([]string, error) {
	var failed bool
	lines, err := o.C.Run(ctx, RollbackCmd(player, seconds, world), 10*time.Minute, func(l string) bool {
		if rollbackOK.MatchString(l) {
			return true
		}
		if rollbackNG.MatchString(l) {
			failed = true
			return true
		}
		return false
	})
	if err == nil && failed {
		err = fmt.Errorf("rollback failed: %s", strings.Join(tail(lines, 3), " / "))
	}
	return lines, err
}

func (o Ops) Kick(ctx context.Context, player, reason string) error {
	_, err := o.C.Run(ctx, fmt.Sprintf("kick %s %s", player, reason), 5*time.Second, nil)
	return err
}

func tail(a []string, n int) []string {
	if len(a) > n {
		return a[len(a)-n:]
	}
	return a
}
