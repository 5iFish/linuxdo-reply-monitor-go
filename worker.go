package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

type Worker struct {
	DB        *sql.DB
	Config    Config
	Discourse *DiscourseClient
}

func (w *Worker) processRule(ctx context.Context, r Rule) error {
	topic, err := w.Discourse.Topic(ctx, r.TopicID)
	if err != nil {
		return err
	}
	if topic.LatestNumber() <= 0 {
		return errors.New("主题缺少有效最高楼层信息，暂不初始化游标")
	}
	title := topic.Title
	if title == "" {
		title = r.Title
	}
	if title == "" {
		title = fmt.Sprintf("主题 %d", r.TopicID)
	}
	if !r.Initialized {
		// 无游标时仅建立基线。即使该主题有几万楼，也不会给用户补发历史邮件。
		if err := commitFetch(w.DB, r, topic.LatestID(), topic.LatestNumber(), title, nil, w.Config.PollInterval); err != nil {
			return err
		}
		log.Printf("规则 %d 已建立初始游标: post_id=%d, floor=%d", r.ID, topic.LatestID(), topic.LatestNumber())
		return nil
	}
	var posts []Post
	newID, newNumber := r.CursorID, r.CursorNumber
	// 部分 Discourse 版本会给 mega topic 返回 [] 而非 null，不能把空流当成“没有新帖”。
	// 正常主题若最高楼层已增长但 ID 流中没有新 ID，也转为按楼层扫描，避免静默漏报。
	ids := newerPostIDs(topic.Stream.IDs, r.CursorID, w.Config.MaxNewPerPoll)
	useIDStream := !topic.Stream.IsMegaTopic && len(topic.Stream.IDs) > 0 && (len(ids) > 0 || topic.LatestNumber() <= r.CursorNumber)
	if useIDStream {
		for start := 0; start < len(ids); start += 20 {
			end := start + 20
			if end > len(ids) {
				end = len(ids)
			}
			batch, err := w.Discourse.Posts(ctx, r.TopicID, ids[start:end])
			if err != nil {
				return err
			}
			byID := make(map[int64]Post, len(batch))
			for _, p := range batch {
				if p.TopicID != 0 && p.TopicID != r.TopicID {
					return fmt.Errorf("帖子 %d 属于其他主题", p.ID)
				}
				byID[p.ID] = p
			}
			for _, id := range ids[start:end] {
				p, ok := byID[id]
				if !ok {
					return fmt.Errorf("帖子 %d 在批量响应中缺失，保留游标等待下次重试", id)
				}
				if p.PostNumber <= 0 {
					return fmt.Errorf("帖子 %d 无有效楼层号", id)
				}
				if p.PostNumber > newNumber {
					newNumber = p.PostNumber
				}
				if p.ID > newID {
					newID = p.ID
				}
				if p.PostNumber > r.CursorNumber && strings.EqualFold(p.Username, r.Username) {
					posts = append(posts, p)
				}
			}
			if end < len(ids) {
				if err := pause(ctx, 900*time.Millisecond); err != nil {
					return err
				}
			}
		}
	} else if topic.LatestNumber() > r.CursorNumber {
		// 巨型主题(10k+ 楼)可能不会返回完整 post_stream.stream，使用按楼层号加载的窗口。
		// 每次请求 cursor+1，因此不依赖 "最后20楼"，高峰期多条回复也能逐窗追赶。
		maxFloor := topic.LatestNumber()
		processed := 0
		for newNumber < maxFloor && processed < w.Config.MaxNewPerPoll {
			window, err := w.Discourse.Window(ctx, r.TopicID, newNumber+1)
			if err != nil {
				return err
			}
			if len(window.Stream.Posts) == 0 {
				return fmt.Errorf("超长主题楼层 %d 窗口为空，停止推进游标", newNumber+1)
			}
			batch := append([]Post(nil), window.Stream.Posts...)
			sort.Slice(batch, func(i, j int) bool { return batch[i].PostNumber < batch[j].PostNumber })
			advanced := false
			for _, p := range batch {
				if p.PostNumber <= newNumber || p.PostNumber > maxFloor {
					continue
				}
				if p.TopicID != 0 && p.TopicID != r.TopicID {
					return fmt.Errorf("窗口帖子 %d 主题错误", p.ID)
				}
				// 给正常的窗口预留少量前文重叠以免主题增删帖子导致边界偏移。
				if p.PostNumber > newNumber+20 && processed == 0 {
					return fmt.Errorf("楼层窗口不连续 (当前 %d, 返回 %d)，不推进游标", newNumber, p.PostNumber)
				}
				newNumber = p.PostNumber
				if p.ID > newID {
					newID = p.ID
				}
				if p.PostNumber > r.CursorNumber && strings.EqualFold(p.Username, r.Username) {
					posts = append(posts, p)
				}
				processed++
				advanced = true
				if processed >= w.Config.MaxNewPerPoll {
					break
				}
			}
			if !advanced {
				return fmt.Errorf("无法从楼层 %d 向后翻页，保留游标避免漏报", newNumber)
			}
			if newNumber < maxFloor && processed < w.Config.MaxNewPerPoll {
				if err := pause(ctx, 900*time.Millisecond); err != nil {
					return err
				}
			}
		}
	}
	// 即便没有目标用户的回复，仍推进扫描游标，避免重复扫描所有人的消息。
	if err := commitFetch(w.DB, r, newID, newNumber, title, posts, w.Config.PollInterval); err != nil {
		return err
	}
	if len(posts) > 0 {
		log.Printf("规则 %d 新增 %d 条目标回复，邮件已入队", r.ID, len(posts))
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Worker) pollRules(ctx context.Context) {
	rules, err := dueRules(w.DB, time.Now().Unix())
	if err != nil {
		log.Printf("查询规则失败: %v", err)
		return
	}
	for _, r := range rules {
		if ctx.Err() != nil {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := w.processRule(runCtx, r)
		cancel()
		if err != nil {
			var apiErr *HTTPError
			var wait time.Duration
			if errors.As(err, &apiErr) {
				wait = apiErr.RetryAfter
			}
			markRuleError(w.DB, r, err, wait)
			log.Printf("规则 %d 获取失败: %v", r.ID, err)
		}
	}
}

func (w *Worker) sendDue(ctx context.Context) {
	pending, err := dueDeliveries(w.DB, time.Now().Unix())
	if err != nil {
		log.Printf("查询邮件队列失败: %v", err)
		return
	}
	for _, d := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := sendDelivery(w.Config, d); err != nil {
			markMailError(w.DB, d, err)
			log.Printf("邮件 %d 发送失败（已安排重试）: %v", d.ID, err)
		} else {
			if err := markSent(w.DB, d.ID); err != nil {
				log.Printf("邮件 %d 状态更新失败: %v", d.ID, err)
			} else {
				log.Printf("邮件 %d 已发送到 SMTP", d.ID)
			}
		}
	}
}
func (w *Worker) Run(ctx context.Context) {
	log.Printf("后台监控启动，检查间隔 %s", w.Config.PollInterval)
	// 采集任务和 SMTP 消费者分开运行；邮件服务卡顿或宕机不阻塞增量抓取。
	var wg sync.WaitGroup
	for _, task := range []func(context.Context){w.pollRules, w.sendDue} {
		wg.Add(1)
		go func(task func(context.Context)) {
			defer wg.Done()
			task(ctx)
			tick := time.NewTicker(10 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					task(ctx)
				}
			}
		}(task)
	}
	wg.Wait()
}
