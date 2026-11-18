package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Rule struct {
	ID           int64
	TopicID      int64
	TopicURL     string
	Username     string
	Enabled      bool
	Initialized  bool
	CursorID     int64
	CursorNumber int64
	Title        string
	NextCheck    int64
	LastChecked  int64
	LastError    string
	FailCount    int
	PendingCount int
	SentCount    int
}

type Delivery struct {
	ID          int64
	RuleID      int64
	PostID      int64
	PostNumber  int64
	Username    string
	TopicTitle  string
	CreatedAt   string
	Cooked      string
	PostURL     string
	Status      string
	Attempts    int
	NextAttempt int64
	LastError   string
	SentAt      int64
}

func openDB(dir string) (*sql.DB, error) {
	path := filepath.Join(dir, "monitor.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	db.Exec("PRAGMA foreign_keys=ON")
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA busy_timeout=30000")
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    topic_id INTEGER NOT NULL,
    topic_url TEXT NOT NULL,
    username TEXT NOT NULL COLLATE NOCASE,
    enabled INTEGER NOT NULL DEFAULT 1,
    initialized INTEGER NOT NULL DEFAULT 0,
    cursor_id INTEGER NOT NULL DEFAULT 0,
    cursor_number INTEGER NOT NULL DEFAULT 0,
    title TEXT NOT NULL DEFAULT '',
    next_check INTEGER NOT NULL DEFAULT 0,
    last_checked INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    fail_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE(topic_id,username)
  )`,
		`CREATE TABLE IF NOT EXISTS deliveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    post_id INTEGER NOT NULL,
    post_number INTEGER NOT NULL,
    username TEXT NOT NULL,
    topic_title TEXT NOT NULL,
    created_at TEXT NOT NULL,
    cooked TEXT NOT NULL,
    post_url TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    sent_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(rule_id,post_id)
  )`,
		`CREATE INDEX IF NOT EXISTS idx_rules_due ON rules(enabled,next_check)`,
		`CREATE INDEX IF NOT EXISTS idx_delivery_due ON deliveries(status,next_attempt)`,
		`CREATE TABLE IF NOT EXISTS app_meta (key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("数据库初始化: %w", err)
		}
	}
	return db, nil
}

func scanRule(rows interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var enabled, initialized int
	err := rows.Scan(&r.ID, &r.TopicID, &r.TopicURL, &r.Username, &enabled, &initialized, &r.CursorID, &r.CursorNumber, &r.Title, &r.NextCheck, &r.LastChecked, &r.LastError, &r.FailCount)
	r.Enabled = enabled != 0
	r.Initialized = initialized != 0
	return r, err
}

const ruleColumns = `id,topic_id,topic_url,username,enabled,initialized,cursor_id,cursor_number,title,next_check,last_checked,last_error,fail_count`

func listRules(db *sql.DB) ([]Rule, error) {
	rows, err := db.Query(`SELECT ` + ruleColumns + ` FROM rules ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE rule_id=? AND status='pending'`, out[i].ID).Scan(&out[i].PendingCount); err != nil {
			return nil, err
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE rule_id=? AND status='sent'`, out[i].ID).Scan(&out[i].SentCount); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func dueRules(db *sql.DB, now int64) ([]Rule, error) {
	rows, err := db.Query(`SELECT `+ruleColumns+` FROM rules WHERE enabled=1 AND next_check<=? ORDER BY next_check,id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func addRule(db *sql.DB, topicID int64, url, name string) error {
	_, err := db.Exec(`INSERT INTO rules(topic_id,topic_url,username,created_at) VALUES(?,?,?,?)`, topicID, url, name, time.Now().Unix())
	return err
}
func editRule(db *sql.DB, id, topicID int64, url, name string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldTopic int64
	var oldName string
	if err := tx.QueryRow(`SELECT topic_id,username FROM rules WHERE id=?`, id).Scan(&oldTopic, &oldName); err != nil {
		return err
	}
	if oldTopic != topicID || !strings.EqualFold(oldName, name) {
		if _, err := tx.Exec(`DELETE FROM deliveries WHERE rule_id=?`, id); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE rules SET topic_id=?,topic_url=?,username=?,initialized=0,cursor_id=0,cursor_number=0,title='',next_check=0,last_checked=0,last_error='',fail_count=0 WHERE id=?`, topicID, url, name, id)
	} else {
		_, err = tx.Exec(`UPDATE rules SET topic_url=?,username=? WHERE id=?`, url, name, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func toggleRule(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE rules SET enabled=1-enabled,next_check=0 WHERE id=?`, id)
	return err
}
func deleteRule(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM rules WHERE id=?`, id)
	return err
}
func checkNow(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE rules SET next_check=0 WHERE id=?`, id)
	return err
}

func seedRule(db *sql.DB, c Config) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists string
	err = tx.QueryRow(`SELECT value FROM app_meta WHERE key='seed_checked'`).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if c.SeedTopicURL != "" {
		topicID, url, err := parseTopicURL(c.SeedTopicURL)
		if err != nil {
			return err
		}
		name, err := normalizedUsername(c.SeedUsername)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO rules(topic_id,topic_url,username,created_at) VALUES(?,?,?,?)`, topicID, url, name, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO app_meta(key,value) VALUES('seed_checked','1')`); err != nil {
		return err
	}
	return tx.Commit()
}

// 抓取游标与待发邮件同事务提交；保护用户同时修改/停用规则的竞态。
func commitFetch(db *sql.DB, old Rule, curID, curNumber int64, title string, posts []Post, interval time.Duration) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	result, err := tx.Exec(`UPDATE rules SET initialized=1,cursor_id=?,cursor_number=?,title=?,last_checked=?,next_check=?,last_error='',fail_count=0 WHERE id=? AND topic_id=? AND username=? COLLATE NOCASE AND enabled=1 AND initialized=? AND cursor_id=? AND cursor_number=?`, curID, curNumber, title, now, now+int64(interval.Seconds()), old.ID, old.TopicID, old.Username, boolInt(old.Initialized), old.CursorID, old.CursorNumber)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return tx.Commit()
	}
	for _, p := range posts {
		if p.ID <= 0 || p.PostNumber <= 0 || strings.TrimSpace(p.Cooked) == "" {
			return fmt.Errorf("帖子 %d 内容不完整，拒绝推进游标", p.ID)
		}
		postURL := fmt.Sprintf("https://linux.do/t/topic/%d/%d", old.TopicID, p.PostNumber)
		_, err = tx.Exec(`INSERT OR IGNORE INTO deliveries(rule_id,post_id,post_number,username,topic_title,created_at,cooked,post_url) VALUES(?,?,?,?,?,?,?,?)`, old.ID, p.ID, p.PostNumber, p.Username, title, p.CreatedAt, p.Cooked, postURL)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func retryDelay(count int, initial, max time.Duration) time.Duration {
	d := initial
	for i := 1; i < count && d < max; i++ {
		d *= 2
		if d > max {
			d = max
		}
	}
	return d
}
func markRuleError(db *sql.DB, old Rule, err error, retryAfter time.Duration) {
	delay := retryDelay(old.FailCount+1, time.Minute, time.Hour)
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	now := time.Now().Unix()
	db.Exec(`UPDATE rules SET fail_count=fail_count+1,last_error=?,last_checked=?,next_check=? WHERE id=? AND topic_id=? AND username=? COLLATE NOCASE AND cursor_number=? AND cursor_id=? AND initialized=?`, truncate(err.Error(), 700), now, now+int64(delay.Seconds()), old.ID, old.TopicID, old.Username, old.CursorNumber, old.CursorID, boolInt(old.Initialized))
}
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
func scanDelivery(scanner interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	err := scanner.Scan(&d.ID, &d.RuleID, &d.PostID, &d.PostNumber, &d.Username, &d.TopicTitle, &d.CreatedAt, &d.Cooked, &d.PostURL, &d.Status, &d.Attempts, &d.NextAttempt, &d.LastError, &d.SentAt)
	return d, err
}

const deliveryCols = `id,rule_id,post_id,post_number,username,topic_title,created_at,cooked,post_url,status,attempts,next_attempt,last_error,sent_at`

func dueDeliveries(db *sql.DB, now int64) ([]Delivery, error) {
	rows, err := db.Query(`SELECT `+deliveryCols+` FROM deliveries WHERE status='pending' AND next_attempt<=? AND rule_id IN (SELECT id FROM rules WHERE enabled=1) ORDER BY id LIMIT 20`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func recentDeliveries(db *sql.DB) ([]Delivery, error) {
	rows, err := db.Query(`SELECT ` + deliveryCols + ` FROM deliveries ORDER BY id DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func markSent(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE deliveries SET status='sent',sent_at=?,last_error='' WHERE id=? AND status='pending'`, time.Now().Unix(), id)
	return err
}
func markMailError(db *sql.DB, d Delivery, err error) {
	delay := retryDelay(d.Attempts+1, time.Minute, 6*time.Hour)
	db.Exec(`UPDATE deliveries SET attempts=attempts+1,last_error=?,next_attempt=? WHERE id=? AND status='pending'`, truncate(err.Error(), 700), time.Now().Add(delay).Unix(), d.ID)
}
