package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Cam は BlueMap のカメラ。URL の # のあとの値と同じ意味 (BlueMapApp.ts の updatePageAddress)。
type Cam struct {
	X, Y, Z float64
	Dist    float64
	Rot     float64 // 向き (ラジアン)
	Angle   float64 // 見下ろす角 (0 が真上、π/2 が水平)
}

func (c Cam) Hash(mapID string) string {
	return fmt.Sprintf("#%s:%.0f:%.0f:%.0f:%.0f:%.2f:%.2f:0:0:perspective", mapID, c.X, c.Y, c.Z, c.Dist, c.Rot, c.Angle)
}

// CamFor は荒らされた場所の中心が画面に収まるカメラ。
// 壊した場所 (x, z ごとの数) の重み付きの中央値を中心にし、遠くに離れた少数の点は無視する (範囲の外れ値で遠くなりすぎないように)。
// 列が無いときは、範囲 (x0..x1, z0..z1) の真ん中を使う。
func CamFor(s *Stats) Cam {
	cx, cz := float64(s.X0+s.X1)/2, float64(s.Z0+s.Z1)/2
	span := math.Max(float64(s.X1-s.X0), float64(s.Z1-s.Z0))
	if len(s.Columns) > 0 {
		cx, cz = weightedMedian(s.Columns, func(c Column) int { return c.X }), weightedMedian(s.Columns, func(c Column) int { return c.Z })
		span = 2 * weightedRadius(s.Columns, cx, cz, 0.6)
	}
	dist := math.Min(math.Max(span*1.1+40, 70), 600)
	y := math.Min(math.Max(float64(s.Y0+s.Y1)/2, 62), 90)
	return Cam{X: cx, Y: y, Z: cz, Dist: dist, Rot: -0.6, Angle: 0.75}
}

func weightedMedian(cols []Column, key func(Column) int) float64 {
	idx := make([]int, len(cols))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return key(cols[idx[a]]) < key(cols[idx[b]]) })
	total := 0
	for _, c := range cols {
		total += c.N
	}
	acc := 0
	for _, i := range idx {
		acc += cols[i].N
		if acc*2 >= total {
			return float64(key(cols[i]))
		}
	}
	return 0
}

// weightedRadius は、中心から、壊した数の p (0〜1) の割合が入る水平の距離。
func weightedRadius(cols []Column, cx, cz, p float64) float64 {
	type dw struct {
		d float64
		n int
	}
	ds := make([]dw, len(cols))
	total := 0
	for i, c := range cols {
		ds[i] = dw{math.Hypot(float64(c.X)-cx, float64(c.Z)-cz), c.N}
		total += c.N
	}
	sort.Slice(ds, func(a, b int) bool { return ds[a].d < ds[b].d })
	acc := 0
	for _, x := range ds {
		acc += x.n
		if float64(acc) >= p*float64(total) {
			return x.d
		}
	}
	return 0
}

// Shooter は BlueMap の web を開いて、画面を撮る (Chromium)。
type Shooter struct {
	URL     string
	Resolve string // "toro-edge:80" のように、URL の名前をこの先へ向ける
	Chrome  string
	Flags   []string
	W, H    int
	MaxWait time.Duration
}

func NewShooter(c *Config) *Shooter {
	var flags []string
	for _, f := range strings.Split(os.Getenv("CHROME_FLAGS"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			flags = append(flags, f)
		}
	}
	return &Shooter{URL: c.BlueMapURL, Resolve: c.BlueMapResolve, Chrome: c.ChromePath, Flags: flags, W: c.ShotWidth, H: c.ShotHeight, MaxWait: c.ShotSettle}
}

// tilesLoadedJS は、いま見えている範囲のタイルが全部読み終わったか。
const tilesLoadedJS = `(() => {
  const b = window.bluemap;
  if (!b || !b.mapViewer || !b.mapViewer.map) return false;
  const m = b.mapViewer.map;
  const mgrs = [m.hiresTileManager].concat(m.lowresTileManager || []);
  let n = 0, loading = 0;
  for (const g of mgrs) {
    if (!g || !g.tiles) continue;
    for (const t of g.tiles.values()) { n++; if (t.loading) loading++; }
  }
  return n > 0 && loading === 0;
})()`

// hideUICSS は、操作の部品 (#app 以下) を消して、地図だけを撮る。
const hideUICSS = `#app{display:none!important}`

// Shot は地図を開いて、タイルが読み終わるまで待ち、PNG を out に書く。
func (s *Shooter) Shot(ctx context.Context, mapID string, cam Cam, out string) error {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.WindowSize(s.W, s.H))
	if s.Chrome != "" {
		opts = append(opts, chromedp.ExecPath(s.Chrome))
	}
	if s.Resolve != "" {
		if u, err := url.Parse(s.URL); err == nil {
			opts = append(opts, chromedp.Flag("host-resolver-rules", fmt.Sprintf("MAP %s %s", u.Hostname(), s.Resolve)))
		}
	}
	for _, f := range s.Flags {
		k, v, hasV := strings.Cut(f, "=")
		if hasV {
			opts = append(opts, chromedp.Flag(k, v))
		} else {
			opts = append(opts, chromedp.Flag(k, true))
		}
	}
	actx, cancelA := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelA()
	cctx, cancelC := chromedp.NewContext(actx)
	defer cancelC()
	cctx, cancelT := context.WithTimeout(cctx, s.MaxWait+90*time.Second)
	defer cancelT()

	target := strings.TrimRight(s.URL, "/") + "/" + cam.Hash(mapID)
	if err := chromedp.Run(cctx, chromedp.Navigate(target), chromedp.WaitVisible("#map-container", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}

	// 読み込み中の見え方が安定するまで (タイルが全部読み終わるまで) 待つ
	deadline := time.Now().Add(s.MaxWait)
	stable := 0
	for time.Now().Before(deadline) {
		var ok bool
		if err := chromedp.Run(cctx, chromedp.Evaluate(tilesLoadedJS, &ok)); err != nil {
			return err
		}
		if ok {
			stable++
			if stable >= 3 {
				break
			}
		} else {
			stable = 0
		}
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-cctx.Done():
			return cctx.Err()
		}
	}

	var buf []byte
	if err := chromedp.Run(cctx,
		chromedp.Evaluate(`(() => { const s = document.createElement('style'); s.textContent = `+"`"+hideUICSS+"`"+`; document.head.appendChild(s); })()`, nil),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.FullScreenshot(&buf, 100), // 100 は PNG
	); err != nil {
		return err
	}
	return os.WriteFile(out, buf, 0o644)
}
