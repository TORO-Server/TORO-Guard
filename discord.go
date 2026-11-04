package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"gopkg.in/yaml.v3"
)

// LoadBotToken は DiscordSRV の config.yml の BotToken を読む。
// このボットは DiscordSRV のものを借りる (トークンを .env やリポジトリに写さない。config.yml を読み取り専用でマウントする)。
func LoadBotToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var y struct {
		BotToken string `yaml:"BotToken"`
	}
	if err := yaml.Unmarshal(b, &y); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	t := strings.TrimSpace(y.BotToken)
	if t == "" || strings.EqualFold(t, "BOTTOKEN") {
		return "", fmt.Errorf("BotToken is empty in %s", path)
	}
	return t, nil
}

// Reporter は通報チャンネル (フォーラム) に投稿する。REST だけを使い、Gateway には接続しない
// (DiscordSRV が同じトークンで Gateway につないでいるので、もう 1 本つながない)。
type Reporter struct {
	s         *discordgo.Session
	channelID string
	tagID     string
}

func NewReporter(token, channelID, rollbackTagID string) (*Reporter, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Client.Timeout = 60 * time.Second
	s.UserAgent = "DiscordBot (TORO-Guard, 1.0)"
	return &Reporter{s: s, channelID: channelID, tagID: rollbackTagID}, nil
}

// Check は投稿せずに、トークンとチャンネルが使えるか確かめる。
func (r *Reporter) Check(ctx context.Context) (string, error) {
	me, err := r.s.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("users/@me: %w", err)
	}
	ch, err := r.s.Channel(r.channelID, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("channel %s: %w", r.channelID, err)
	}
	if ch.Type != discordgo.ChannelTypeGuildForum {
		return "", fmt.Errorf("channel %s is not a forum (type %d)", r.channelID, ch.Type)
	}
	return fmt.Sprintf("bot=%s channel=%s (forum)", me.Username, ch.Name), nil
}

type ReportFile struct {
	Name string
	Path string
}

// Post は新しい投稿を立てる。existing があれば、その投稿に書き足す。投稿の ID を返す。
// rolledBack のときは、投稿に「ロルバ済」のタグを付ける。
func (r *Reporter) Post(ctx context.Context, existing, title, content string, files []ReportFile, rolledBack bool) (string, error) {
	var dfiles []*discordgo.File
	var closers []*os.File
	defer func() {
		for _, f := range closers {
			f.Close()
		}
	}()
	for _, f := range files {
		fh, err := os.Open(f.Path)
		if err != nil {
			return "", err
		}
		closers = append(closers, fh)
		dfiles = append(dfiles, &discordgo.File{Name: f.Name, Reader: fh})
	}
	msg := &discordgo.MessageSend{Content: content, Files: dfiles}
	var tags []string
	if rolledBack && r.tagID != "" {
		tags = append(tags, r.tagID)
	}

	if existing != "" {
		if _, err := r.s.ChannelMessageSendComplex(existing, msg, discordgo.WithContext(ctx)); err == nil {
			if len(tags) > 0 {
				_, _ = r.s.ChannelEdit(existing, &discordgo.ChannelEdit{AppliedTags: &tags}, discordgo.WithContext(ctx))
			}
			return existing, nil
		}
		// 投稿が消えている・書けないときは、新しく立てる
	}
	th, err := r.s.ForumThreadStartComplex(r.channelID, &discordgo.ThreadStart{
		Name:                title,
		AutoArchiveDuration: 10080,
		AppliedTags:         tags,
	}, msg, discordgo.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return th.ID, nil
}
