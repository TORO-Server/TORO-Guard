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
// review は、画像と報告文を作って「確認待ち」に置き、人か AI (Muse など) が窓口 (control) から
// 通報・ロールバック・却下を決める。自分では、投稿もロールバックもキックもしない。
type Mode string

const (
	ModeObserve Mode = "observe"
	ModeDryRun  Mode = "dry-run"
	ModeReview  Mode = "review"
	ModeLive    Mode = "live"
)

// CanAct は、窓口 (control) からの操作 (通報・ロールバック・ミュート・キック) を受け付ける動かし方か。
func (m Mode) CanAct() bool { return m == ModeReview || m == ModeLive }

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

	// チャットのスパム (CoreProtect の chat の表)
	ChatEnabled      bool
	ChatRate         int           // この数以上の発言を
	ChatRateWindow   time.Duration // この時間のうちにしたら (連投)
	ChatRepeat       int           // 同じ (ほぼ同じ) 発言を、この数以上
	ChatRepeatWindow time.Duration // この時間のうちにしたら (コピペの連投)
	ChatRun          int           // 同じ文字がこの数以上つづく発言 (あああああ…)
	ChatLinkRepeat   int           // リンクつきの発言を、この数以上 (5 分のうちに)
	ChatBadWordsFile string        // 1 行に 1 語。発言に入っていたら、1 回で通報する (空なら使わない)
	ChatQuiet        time.Duration // 発言が止まって、この時間たったら通報する
	ChatMuteCmd      string        // 例: "mute {name} 10m spam"。空なら、ミュートはしない (live だけ)
	ChatMuteTagID    string        // ミュートしたときに、投稿に付けるタグ (空なら付けない)

	// AI (Claude の Messages API)。キーが無ければ、AI は使わない
	AIKey      string
	AIBaseURL  string
	AIModel    string
	AIMinConf  int  // この確からしさ (0〜100) 以上でないと、AI の判定を採らない
	AIRequired bool // true なら、AI が「荒らし」と答えないかぎり、ロールバックしない (AI が使えないときも)
	AIChat     bool // チャットの発言を AI にも見せる

	// Discord
	DiscordSRVConfig string
	ReportChannelID  string
	RollbackTagID    string
	DiscordMCPURL    string // Discord-MCP (Streamable HTTP) の /mcp
	DiscordMCPLazy   bool   // サーバーが DISCORD_MCP_LAZY_TOOLS=true (discord_call_tool 1 本) か
	MCPProtocol      string // Mcp-Protocol-Version

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

	// 操作の窓口 (TORO-MCP が、Admin 以上の人の頼みを、その人のトークンのまま取り次ぐ)。内部のネットワークだけに出す。
	// 認証は OAuth 2.0 のトークン: auth の確認の窓口 (/introspect) に尋ね、いまの Discord のロールが AdminRoles のどれかであること。
	// ControlAddr が空なら窓口は開かない
	ControlAddr      string
	AuthInternalURL  string   // auth の確認の窓口 (内部のネットワークだけ)
	AdminRoles       []string // 窓口を使える Discord のロールの ID (Owner と Admin)
	RequireLiveRoles bool     // true なら、auth がいまの Discord で確かめたロール (roles_source=live) だけを信じる
	DiscordAllow     string   // 窓口から呼んでよい Discord のツール名の正規表現

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
		AIKey:            firstEnv("AI_API_KEY", "ANTHROPIC_API_KEY"),
		AIBaseURL:        envStr("AI_BASE_URL", "https://api.anthropic.com"),
		AIModel:          envStr("AI_MODEL", "claude-haiku-5-5"),
		AIMinConf:        envInt("AI_MIN_CONFIDENCE", 70),
		AIRequired:       envBool("AI_REQUIRED", false),
		AIChat:           envBool("AI_CHAT", true),
		ChatEnabled:      envBool("CHAT_ENABLED", true),
		ChatRate:         envInt("CHAT_RATE", 12),
		ChatRateWindow:   envDur("CHAT_RATE_WINDOW", 10*time.Second),
		ChatRepeat:       envInt("CHAT_REPEAT", 5),
		ChatRepeatWindow: envDur("CHAT_REPEAT_WINDOW", 60*time.Second),
		ChatRun:          envInt("CHAT_RUN", 30),
		ChatLinkRepeat:   envInt("CHAT_LINK_REPEAT", 3),
		ChatBadWordsFile: os.Getenv("CHAT_BAD_WORDS_FILE"),
		ChatQuiet:        envDur("CHAT_QUIET", 30*time.Second),
		ChatMuteCmd:      os.Getenv("CHAT_MUTE_CMD"),
		ChatMuteTagID:    os.Getenv("CHAT_MUTE_TAG_ID"),
		DiscordSRVConfig: envStr("DISCORDSRV_CONFIG", "/discordsrv/config.yml"),
		ReportChannelID:  envStr("REPORT_CHANNEL_ID", "1000000000000000001"),
		RollbackTagID:    envStr("ROLLBACK_TAG_ID", "1000000000000000002"),
		DiscordMCPURL:    envStr("DISCORD_MCP_URL", "http://discord-mcp:8085/mcp"),
		DiscordMCPLazy:   envBool("DISCORD_MCP_LAZY", true),
		MCPProtocol:      envStr("MCP_PROTOCOL_VERSION", "2025-11-25"),
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
		ControlAddr:      envStr("CONTROL_ADDR", ":8090"),
		AuthInternalURL:  strings.TrimRight(envStr("AUTH_INTERNAL_URL", "http://toro-auth:8081"), "/"),
		AdminRoles:       splitList(envStr("ADMIN_ROLE_IDS", defaultAdminRoles)),
		RequireLiveRoles: envBool("CONTROL_REQUIRE_LIVE_ROLES", true),
		DiscordAllow:     envStr("CONTROL_DISCORD_ALLOW", defaultDiscordAllow),
		WarpsDir:         os.Getenv("WARPS_DIR"),
		DataDir:          envStr("DATA_DIR", "/data"),
	}
	switch c.Mode {
	case ModeObserve, ModeDryRun, ModeReview, ModeLive:
	default:
		return nil, fmt.Errorf("GUARD_MODE must be observe, dry-run, review or live: %q", c.Mode)
	}
	return c, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
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

// defaultAdminRoles は、窓口を使える Discord のロール (TORO サーバーの Owner と Admin)。
const defaultAdminRoles = "100000000000000001,100000000000000002"

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
