package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode は動かし方。observe は検知して記録するだけ、dry-run は画像と報告文まで作って手元に置くだけ
// (Discord には投稿せず、ロールバックもしない)、live は投稿とロールバックまで行う。
type Mode string

const (
	ModeObserve Mode = "observe"
	ModeDryRun  Mode = "dry-run"
	ModeLive    Mode = "live"
)

type Config struct {
	Mode Mode

	// CoreProtect (TiDB)
	DBHost      string
	DBPort      string
	DBUser      string
	DBPassword  string
	DBName      string
	TablePrefix string

	// 検知の閾値
	BurstWindow     time.Duration // この秒数のあいだに
	BreakBurst      int           // これ以上壊したら
	PlaceBurst      int           // これ以上置いたら
	SustainedWindow time.Duration
	SustainedBreaks int // 長い窓でこれ以上壊したら
	Quiet           time.Duration
	MaxIncident     time.Duration
	WEGrace         time.Duration // WorldEdit などのコマンドを使って、この時間は検知しない
	TrustedPlayers  map[string]bool
	OpsFile         string // op の一覧 (ops.json)。op も信頼する人として扱う
	PollInterval    time.Duration
	MaxEventAge     time.Duration // これより古い行は、見ても報告しない (止まっていたあいだのもの)

	// Discord
	DiscordSRVConfig string
	ReportChannelID  string
	RollbackTagID    string

	// Minecraft のコンソール (docker attach)
	DockerSocket string
	MCContainer  string
	KickEnabled  bool

	// BlueMap の画面
	BlueMapURL     string // 例: http://3d-map.torosaba.net
	BlueMapResolve string // 例: toro-edge:80 (Host の名前をこの先に向ける)。空ならそのまま
	MapIDs         map[string]string
	ChromePath     string
	ShotWidth      int
	ShotHeight     int
	ShotSettle     time.Duration

	WarpsDir string
	DataDir  string
}

func loadConfig() (*Config, error) {
	c := &Config{
		Mode:             Mode(envStr("GUARD_MODE", "observe")),
		DBHost:           envStr("DB_HOST", "tidb"),
		DBPort:           envStr("DB_PORT", "4000"),
		DBUser:           envStr("DB_USER", "root"),
		DBPassword:       os.Getenv("DB_PASSWORD"),
		DBName:           envStr("DB_NAME", "coreprotect"),
		TablePrefix:      envStr("DB_TABLE_PREFIX", "coreprotect_"),
		BurstWindow:      envDur("BURST_WINDOW", 5*time.Second),
		BreakBurst:       envInt("BREAK_BURST", 100),
		PlaceBurst:       envInt("PLACE_BURST", 150),
		SustainedWindow:  envDur("SUSTAINED_WINDOW", 10*time.Minute),
		SustainedBreaks:  envInt("SUSTAINED_BREAKS", 1500),
		Quiet:            envDur("QUIET", 90*time.Second),
		MaxIncident:      envDur("MAX_INCIDENT", 10*time.Minute),
		WEGrace:          envDur("WE_GRACE", 30*time.Minute),
		TrustedPlayers:   lowerSet(os.Getenv("TRUSTED_PLAYERS")),
		OpsFile:          os.Getenv("OPS_FILE"),
		PollInterval:     envDur("POLL_INTERVAL", 3*time.Second),
		MaxEventAge:      envDur("MAX_EVENT_AGE", 15*time.Minute),
		DiscordSRVConfig: envStr("DISCORDSRV_CONFIG", "/discordsrv/config.yml"),
		ReportChannelID:  envStr("REPORT_CHANNEL_ID", "1000000000000000001"),
		RollbackTagID:    envStr("ROLLBACK_TAG_ID", "1000000000000000002"),
		DockerSocket:     envStr("DOCKER_SOCKET", "/var/run/docker.sock"),
		MCContainer:      envStr("MC_CONTAINER", "toro-main"),
		KickEnabled:      envBool("KICK_ENABLED", false),
		BlueMapURL:       envStr("BLUEMAP_URL", "http://3d-map.torosaba.net"),
		BlueMapResolve:   os.Getenv("BLUEMAP_RESOLVE"),
		MapIDs:           parseMap(envStr("MAP_IDS", "main=overworld")),
		ChromePath:       os.Getenv("CHROME_PATH"),
		ShotWidth:        envInt("SHOT_WIDTH", 1280),
		ShotHeight:       envInt("SHOT_HEIGHT", 720),
		ShotSettle:       envDur("SHOT_SETTLE", 60*time.Second),
		WarpsDir:         os.Getenv("WARPS_DIR"),
		DataDir:          envStr("DATA_DIR", "/data"),
	}
	switch c.Mode {
	case ModeObserve, ModeDryRun, ModeLive:
	default:
		return nil, fmt.Errorf("GUARD_MODE must be observe, dry-run or live: %q", c.Mode)
	}
	return c, nil
}

func envStr(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envBool(k string, d bool) bool {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return d
}

func envDur(k string, d time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if x, err := time.ParseDuration(v); err == nil {
			return x
		}
	}
	return d
}

func lowerSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			m[p] = true
		}
	}
	return m
}

// parseMap は "main=overworld,flat=flat" をワールド名 → BlueMap の地図の名前にする。
func parseMap(s string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}
