package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func parseTopicURL(raw string) (int64, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (strings.ToLower(u.Hostname()) != "linux.do" && strings.ToLower(u.Hostname()) != "www.linux.do") {
		return 0, "", errors.New("请输入 https://linux.do/t/... 主题链接")
	}
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 2 || seg[0] != "t" {
		return 0, "", errors.New("主题路径格式不正确")
	}
	var rawID string
	// /t/2696141/33528 和 /t/topic/2696141/33528 都支持。
	if _, e := strconv.ParseInt(seg[1], 10, 64); e == nil {
		rawID = seg[1]
	} else if len(seg) >= 3 {
		rawID = seg[2]
	} else {
		return 0, "", errors.New("主题链接缺少数字 ID")
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return 0, "", errors.New("无法从主题链接中解析主题 ID")
	}
	return id, fmt.Sprintf("https://linux.do/t/topic/%d", id), nil
}
func normalizedUsername(s string) (string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "@"))
	if !usernamePattern.MatchString(s) {
		return "", errors.New("用户名长度须为 1-64，只能包含字母数字 . _ -")
	}
	return s, nil
}

type Post struct {
	ID         int64  `json:"id"`
	TopicID    int64  `json:"topic_id"`
	PostNumber int64  `json:"post_number"`
	Username   string `json:"username"`
	Cooked     string `json:"cooked"`
	CreatedAt  string `json:"created_at"`
	PostType   int    `json:"post_type"`
}

type Topic struct {
	ID                int64  `json:"id"`
	Title             string `json:"title"`
	HighestPostNumber int64  `json:"highest_post_number"`
	Stream            struct {
		IDs         []int64 `json:"stream"`
		Posts       []Post  `json:"posts"`
		LastID      int64   `json:"lastId"`
		IsMegaTopic bool    `json:"isMegaTopic"`
	} `json:"post_stream"`
}

func (t Topic) LatestNumber() int64 {
	m := t.HighestPostNumber
	for _, p := range t.Stream.Posts {
		if p.PostNumber > m {
			m = p.PostNumber
		}
	}
	return m
}
func (t Topic) LatestID() int64 {
	m := t.Stream.LastID
	for _, id := range t.Stream.IDs {
		if id > m {
			m = id
		}
	}
	for _, p := range t.Stream.Posts {
		if p.ID > m {
			m = p.ID
		}
	}
	return m
}

type HTTPError struct {
	Status     int
	RetryAfter time.Duration
	Text       string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("Linux.do HTTP %d: %s", e.Status, e.Text) }

type DiscourseClient struct {
	Client              *http.Client
	APIKey, APIUsername string
}

func newDiscourseClient(c Config) *DiscourseClient {
	return &DiscourseClient{Client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" || (req.URL.Hostname() != "linux.do" && req.URL.Hostname() != "www.linux.do") {
			return http.ErrUseLastResponse
		}
		return nil
	}}, APIKey: c.DiscourseAPIKey, APIUsername: c.DiscourseAPIUsername}
}
func (d *DiscourseClient) getJSON(ctx context.Context, requestURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LinuxDoReplyMonitor/1.0 (periodic public Discourse API monitoring)")
	if d.APIKey != "" {
		req.Header.Set("Api-Key", d.APIKey)
		req.Header.Set("Api-Username", d.APIUsername)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		msg := strings.TrimSpace(string(b))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		if msg == "" {
			msg = resp.Status
		}
		retry := time.Duration(0)
		if n, e := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); e == nil && n > 0 {
			retry = time.Duration(n) * time.Second
		} else if at, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			retry = time.Until(at)
		}
		return &HTTPError{Status: resp.StatusCode, RetryAfter: retry, Text: msg}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "json") {
		return fmt.Errorf("响应非 JSON (%s)，可能被登录或 Cloudflare 页面拦截", ct)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	return nil
}
func (d *DiscourseClient) Topic(ctx context.Context, id int64) (Topic, error) {
	return d.topicURL(ctx, fmt.Sprintf("https://linux.do/t/%d.json", id), id)
}
func (d *DiscourseClient) Window(ctx context.Context, id, number int64) (Topic, error) {
	return d.topicURL(ctx, fmt.Sprintf("https://linux.do/t/%d.json?post_number=%d", id, number), id)
}
func (d *DiscourseClient) topicURL(ctx context.Context, u string, id int64) (Topic, error) {
	var t Topic
	err := d.getJSON(ctx, u, &t)
	if err != nil {
		return t, err
	}
	if t.ID != id {
		return t, fmt.Errorf("主题 ID 不匹配：期望 %d 实际 %d", id, t.ID)
	}
	if t.Stream.Posts == nil && t.Stream.IDs == nil {
		return t, errors.New("响应缺少 post_stream.posts 和 stream，无法安全继续")
	}
	return t, nil
}
func (d *DiscourseClient) Posts(ctx context.Context, topicID int64, ids []int64) ([]Post, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 20 {
		return nil, errors.New("单次最多请求 20 条")
	}
	q := url.Values{}
	for _, id := range ids {
		q.Add("post_ids[]", strconv.FormatInt(id, 10))
	}
	var payload struct {
		PostStream struct {
			Posts []Post `json:"posts"`
		} `json:"post_stream"`
	}
	u := fmt.Sprintf("https://linux.do/t/%d/posts.json?%s", topicID, q.Encode())
	if err := d.getJSON(ctx, u, &payload); err != nil {
		return nil, err
	}
	if payload.PostStream.Posts == nil {
		return nil, errors.New("返回帖子详情缺失，暂不推进游标")
	}
	return payload.PostStream.Posts, nil
}
func newerPostIDs(stream []int64, cursor int64, limit int) []int64 {
	ids := make([]int64, 0)
	seen := make(map[int64]bool)
	for _, id := range stream {
		if id > cursor && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}
