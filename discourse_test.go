package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(s string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(s))}
}
func TestParseTopicURLs(t *testing.T) {
	for _, raw := range []string{"https://linux.do/t/topic/2696141/33528", "https://linux.do/t/2696141/33528", "https://linux.do/t/2696141", "https://www.linux.do/t/topic/2696141"} {
		id, canonical, err := parseTopicURL(raw)
		if err != nil || id != 2696141 || canonical != "https://linux.do/t/topic/2696141" {
			t.Fatalf("%s => %d %s %v", raw, id, canonical, err)
		}
	}
	for _, raw := range []string{"https://evil.com/t/topic/1", "http://linux.do/t/1", "https://linux.do.evil.com/t/1", "https://linux.do/t/topic/invalid"} {
		if _, _, err := parseTopicURL(raw); err == nil {
			t.Errorf("应拒绝 %s", raw)
		}
	}
}
func TestNewerPostIDs(t *testing.T) {
	ids := newerPostIDs([]int64{3, 1, 4, 2, 4, 9}, 2, 3)
	expected := []int64{3, 4, 9}
	for i, v := range expected {
		if ids[i] != v {
			t.Fatalf("结果 %v", ids)
		}
	}
}
func TestMegaTopicNoCursorAndCatchup(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := addRule(db, 2696141, "https://linux.do/t/topic/2696141", "Andrew-hh"); err != nil {
		t.Fatal(err)
	}
	var highest int64 = 33528
	source := &DiscourseClient{Client: &http.Client{Timeout: time.Second, Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.RawQuery, "post_number=") {
			return jsonResponse(`{"id":2696141,"title":"长帖子","highest_post_number":33530,"post_stream":{"isMegaTopic":true,"posts":[{"id":990,"topic_id":2696141,"post_number":33528,"username":"old","cooked":"<p>旧消息</p>"},{"id":991,"topic_id":2696141,"post_number":33529,"username":"Andrew-hh","cooked":"<p>新回复 &amp; 链接</p>","created_at":"2026-11-18T03:00:00Z"},{"id":992,"topic_id":2696141,"post_number":33530,"username":"Someone","cooked":"<p>其他人</p>"}]}}`), nil
		}
		return jsonResponse(fmt.Sprintf(`{"id":2696141,"title":"长帖子","highest_post_number":%d,"post_stream":{"isMegaTopic":true,"lastId":990,"posts":[{"id":1,"post_number":1,"cooked":"<p>开场</p>"}]}}`, highest)), nil
	})}}
	w := &Worker{DB: db, Config: Config{PollInterval: time.Minute, MaxNewPerPoll: 100}, Discourse: source}
	rules, err := listRules(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.processRule(context.Background(), rules[0]); err != nil {
		t.Fatal(err)
	}
	rules, _ = listRules(db)
	if !rules[0].Initialized || rules[0].CursorNumber != 33528 {
		t.Fatalf("基线错误: %+v", rules[0])
	}
	deliveries, _ := recentDeliveries(db)
	if len(deliveries) != 0 {
		t.Fatal("首次无游标不应发送历史消息")
	}
	highest = 33530
	if err := w.processRule(context.Background(), rules[0]); err != nil {
		t.Fatal(err)
	}
	rules, _ = listRules(db)
	deliveries, _ = recentDeliveries(db)
	if rules[0].CursorNumber != 33530 || len(deliveries) != 1 || deliveries[0].Username != "Andrew-hh" || deliveries[0].PostURL != "https://linux.do/t/topic/2696141/33529" {
		t.Fatalf("增量抓取失败: %+v %v", rules, deliveries)
	}
	if err := w.processRule(context.Background(), rules[0]); err != nil {
		t.Fatal(err)
	}
	deliveries, _ = recentDeliveries(db)
	if len(deliveries) != 1 {
		t.Fatal("去重失败")
	}
}
func TestMailErrorKeepsQueue(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := addRule(db, 1, "https://linux.do/t/topic/1", "testuser"); err != nil {
		t.Fatal(err)
	}
	r, _ := listRules(db)
	if err := commitFetch(db, r[0], 42, 10, "测试主题", []Post{{ID: 42, PostNumber: 10, Username: "testuser", Cooked: "<p>内容</p>"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	pending, _ := dueDeliveries(db, time.Now().Unix())
	if len(pending) != 1 {
		t.Fatalf("应有一个邮件任务: %v", pending)
	}
	markMailError(db, pending[0], fmt.Errorf("断开连接"))
	pending, _ = dueDeliveries(db, time.Now().Unix())
	if len(pending) != 0 {
		t.Fatal("失败邮件应退避")
	}
	all, _ := recentDeliveries(db)
	if len(all) != 1 || all[0].Status != "pending" || all[0].Attempts != 1 {
		t.Fatalf("队列未保留: %+v", all)
	}
}

func TestMegaTopicEmptyStreamNotSkipped(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := addRule(db, 2696141, "https://linux.do/t/topic/2696141", "Andrew-hh"); err != nil {
		t.Fatal(err)
	}
	var highest int64 = 40010
	windows := 0
	source := &DiscourseClient{Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.RawQuery, "post_number=") {
			windows++
			return jsonResponse(`{"id":2696141,"title":"超长帖子","highest_post_number":40012,"post_stream":{"stream":[],"isMegaTopic":true,"posts":[
				{"id":600101,"topic_id":2696141,"post_number":40010,"username":"other","cooked":"<p>以前的</p>"},
				{"id":600102,"topic_id":2696141,"post_number":40011,"username":"Andrew-hh","cooked":"<p>需要通知</p>","created_at":"2026-11-18T03:00:00Z"},
				{"id":600103,"topic_id":2696141,"post_number":40012,"username":"other","cooked":"<p>其他人</p>"}]}}`), nil
		}
		return jsonResponse(fmt.Sprintf(`{"id":2696141,"title":"超长帖子","highest_post_number":%d,"post_stream":{"stream":[],"isMegaTopic":true,"lastId":600101,"posts":[]}}`, highest)), nil
	})}}
	w := &Worker{DB: db, Config: Config{PollInterval: time.Minute, MaxNewPerPoll: 100}, Discourse: source}
	r, _ := listRules(db)
	if err := w.processRule(context.Background(), r[0]); err != nil {
		t.Fatal(err)
	}
	highest = 40012
	r, _ = listRules(db)
	if err := w.processRule(context.Background(), r[0]); err != nil {
		t.Fatal(err)
	}
	if windows == 0 {
		t.Fatal("空 ID stream 且楼层增长时必须调用按楼层窗口 API")
	}
	r, _ = listRules(db)
	deliveries, _ := recentDeliveries(db)
	if r[0].CursorNumber != 40012 || len(deliveries) != 1 || deliveries[0].PostNumber != 40011 {
		t.Fatalf("空 stream 漏报或游标错误: rule=%+v mails=%+v", r[0], deliveries)
	}
}

func TestHTTPFailureMustNotInitializeCursor(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := addRule(db, 1, "https://linux.do/t/topic/1", "Andrew-hh"); err != nil {
		t.Fatal(err)
	}
	source := &DiscourseClient{Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader("slow down"))}, nil
	})}}
	w := &Worker{DB: db, Config: Config{PollInterval: time.Minute, MaxNewPerPoll: 100}, Discourse: source}
	r, _ := listRules(db)
	err = w.processRule(context.Background(), r[0])
	var apiErr *HTTPError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.RetryAfter != 120*time.Second {
		t.Fatalf("HTTP 429 和 Retry-After 解析失败: %v", err)
	}
	r, _ = listRules(db)
	if r[0].Initialized || r[0].CursorID != 0 || r[0].CursorNumber != 0 {
		t.Fatalf("网络失败不能建立游标: %+v", r[0])
	}
}

func TestNormalTopicStreamFiltersAndDeduplicates(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := addRule(db, 123, "https://linux.do/t/topic/123", "Andrew-hh"); err != nil {
		t.Fatal(err)
	}
	var now int64 = 5
	postsRequested := 0
	source := &DiscourseClient{Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/posts.json") {
			postsRequested++
			return jsonResponse(`{"post_stream":{"posts":[
				{"id":601,"topic_id":123,"post_number":6,"username":"Andrew-hh","cooked":"<p>新回复</p>","created_at":"2026-11-18T00:00:00Z"},
				{"id":602,"topic_id":123,"post_number":7,"username":"someone","cooked":"<p>其他作者</p>"}
			]}}`), nil
		}
		stream := "101,102,103,104,105"
		if now >= 7 {
			stream += ",601,602"
		}
		return jsonResponse(fmt.Sprintf(`{"id":123,"title":"普通主题","highest_post_number":%d,"post_stream":{"stream":[%s],"posts":[]}}`, now, stream)), nil
	})}}
	w := &Worker{DB: db, Config: Config{PollInterval: time.Minute, MaxNewPerPoll: 100}, Discourse: source}
	r, _ := listRules(db)
	if err := w.processRule(context.Background(), r[0]); err != nil {
		t.Fatal(err)
	}
	now = 7
	r, _ = listRules(db)
	if err := w.processRule(context.Background(), r[0]); err != nil {
		t.Fatal(err)
	}
	r, _ = listRules(db)
	if err := w.processRule(context.Background(), r[0]); err != nil {
		t.Fatal(err)
	}
	deliveries, _ := recentDeliveries(db)
	if postsRequested != 1 || len(deliveries) != 1 || deliveries[0].PostID != 601 {
		t.Fatalf("普通流筛选/去重失败: requests=%d deliveries=%+v", postsRequested, deliveries)
	}
}

func TestEmailHTMLPreservesRelativeLinksAndImages(t *testing.T) {
	html := safeHTML(`<p><a href="/t/topic/2696141/33529">看原帖</a><img src="/uploads/default/original/1x/abc.png"><script>alert(1)</script></p>`)
	if !strings.Contains(html, "https://linux.do/t/topic/2696141/33529") || !strings.Contains(html, "https://linux.do/uploads/default/original/1x/abc.png") {
		t.Fatalf("相对链接和图片未补全: %s", html)
	}
	if strings.Contains(html, "<script") {
		t.Fatalf("不安全 HTML 未被清理: %s", html)
	}
}
