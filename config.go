package main

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen, DataDir, AdminUser, AdminPassword                                 string
	SMTPHost, SMTPPort, SMTPUser, SMTPPassword, SMTPFrom, SMTPTo, SMTPTLSMode string
	DiscourseAPIKey, DiscourseAPIUsername                                     string
	SeedTopicURL, SeedUsername                                                string
	PollInterval                                                              time.Duration
	MaxNewPerPoll                                                             int
}

func env(k, d string) string {
	if v, ok := os.LookupEnv(k); ok {
		return strings.TrimSpace(v)
	}
	return d
}
func loadConfig() (Config, error) {
	c := Config{
		Listen: env("LISTEN_ADDR", ":8080"), DataDir: env("DATA_DIR", "/data"), AdminUser: env("ADMIN_USER", "admin"), AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		SMTPHost: env("SMTP_HOST", ""), SMTPPort: env("SMTP_PORT", "587"), SMTPUser: env("SMTP_USER", ""), SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom: env("SMTP_FROM", ""), SMTPTo: env("SMTP_TO", ""), SMTPTLSMode: strings.ToLower(env("SMTP_TLS_MODE", "starttls")),
		DiscourseAPIKey: env("DISCOURSE_API_KEY", ""), DiscourseAPIUsername: env("DISCOURSE_API_USERNAME", ""),
		SeedTopicURL: env("SEED_TOPIC_URL", ""), SeedUsername: env("SEED_USERNAME", ""),
	}
	if len(c.AdminPassword) < 12 || strings.Contains(strings.ToLower(c.AdminPassword), "replace-with") || strings.Contains(c.AdminPassword, "CHANGE_ME") {
		return c, errors.New("ADMIN_PASSWORD 必须改成至少 12 位的自定义强密码，不能使用示例占位符")
	}
	if c.SMTPHost == "" || c.SMTPFrom == "" || c.SMTPTo == "" {
		return c, errors.New("SMTP_HOST / SMTP_FROM / SMTP_TO 均不能为空")
	}
	if (c.SMTPUser == "") != (c.SMTPPassword == "") {
		return c, errors.New("SMTP_USER 和 SMTP_PASSWORD 必须同时配置")
	}
	if _, err := mail.ParseAddress(c.SMTPFrom); err != nil {
		return c, fmt.Errorf("SMTP_FROM 非法: %w", err)
	}
	if _, err := mail.ParseAddressList(c.SMTPTo); err != nil {
		return c, fmt.Errorf("SMTP_TO 非法: %w", err)
	}
	port, err := strconv.Atoi(c.SMTPPort)
	if err != nil || port < 1 || port > 65535 {
		return c, errors.New("SMTP_PORT 非法")
	}
	if c.SMTPTLSMode != "starttls" && c.SMTPTLSMode != "implicit" {
		return c, errors.New("SMTP_TLS_MODE 只能为 starttls 或 implicit")
	}
	if (c.DiscourseAPIKey == "") != (c.DiscourseAPIUsername == "") {
		return c, errors.New("DISCOURSE_API_KEY 与 DISCOURSE_API_USERNAME 必须同时配置")
	}
	c.PollInterval, err = time.ParseDuration(env("POLL_INTERVAL", "5m"))
	if err != nil || c.PollInterval < time.Minute {
		return c, errors.New("POLL_INTERVAL 最短 1m")
	}
	c.MaxNewPerPoll, err = strconv.Atoi(env("MAX_NEW_PER_POLL", "100"))
	if err != nil || c.MaxNewPerPoll < 20 || c.MaxNewPerPoll > 1000 {
		return c, errors.New("MAX_NEW_PER_POLL 范围 20-1000")
	}
	if (c.SeedTopicURL == "") != (c.SeedUsername == "") {
		return c, errors.New("SEED_TOPIC_URL 和 SEED_USERNAME 须同时配置")
	}
	if c.SeedTopicURL != "" {
		if _, _, err := parseTopicURL(c.SeedTopicURL); err != nil {
			return c, err
		}
		if _, err := normalizedUsername(c.SeedUsername); err != nil {
			return c, err
		}
	}
	return c, nil
}
