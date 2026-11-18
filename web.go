package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type WebApp struct {
	DB        *sql.DB
	Config    Config
	Discourse *DiscourseClient
	CSRF      string
}
type pageData struct {
	Rules         []Rule
	Deliveries    []Delivery
	Config        Config
	CSRF, Message string
}

var pageTemplate = template.Must(template.New("index").Funcs(template.FuncMap{
	"when": func(t int64) string {
		if t <= 0 {
			return "—"
		}
		return time.Unix(t, 0).Format("2006-01-02 15:04:05")
	},
	"short": func(s string) string {
		if len([]rune(s)) > 110 {
			return string([]rune(s)[:110]) + "…"
		}
		return s
	},
}).Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Linux.do 回复监控</title><style>
:root{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;color:#1b2639;background:#f3f6fa}*{box-sizing:border-box}body{margin:0;padding:0 16px 50px}.wrap{max-width:1150px;margin:auto}h1{margin:32px 0 7px;font-size:27px}.sub{color:#64748b;margin-bottom:23px}.panel{background:#fff;border:1px solid #dbe3ec;box-shadow:0 1px 3px #00000006;border-radius:13px;margin:18px 0;padding:20px}h2{font-size:19px;margin:0 0 15px}.grid{display:grid;grid-template-columns:3fr 1.5fr auto;gap:10px;align-items:end}label{display:block;font-size:13px;color:#475569;margin-bottom:6px;font-weight:600}input{border:1px solid #becbd9;border-radius:8px;font:inherit;padding:10px;width:100%;background:#fff}input:focus{outline:2px solid #95bbed}button{cursor:pointer;border:0;border-radius:8px;background:#2563eb;color:#fff;padding:10px 14px;font:inherit}button.gray{background:#475569}button.red{background:#b91c1c}button.green{background:#15803d}button.small{font-size:13px;padding:7px 9px}.notice{background:#eff6ff;border:1px solid #bfdbfe;border-radius:8px;padding:11px 14px}.scroll{overflow-x:auto}table{width:100%;border-collapse:collapse;font-size:13px;text-align:left}th,td{padding:12px 9px;border-bottom:1px solid #e5edf5;vertical-align:top}th{color:#64748b;white-space:nowrap}td strong{font-size:14px}td a{color:#1d4ed8;word-break:break-all}small,.muted{font-size:12px;color:#64748b}.bad{color:#b91c1c;max-width:240px;word-break:break-word}.ok{color:#15803d}.tag{border-radius:30px;padding:3px 7px;background:#e2e8f0;color:#334155;font-size:12px;white-space:nowrap}.tag.green{background:#dcfce7;color:#166534}.actions{display:flex;flex-wrap:wrap;gap:5px;margin-top:7px}.actions form{margin:0}details{border-top:1px solid #e2e8f0;padding-top:7px;margin-top:7px}summary{cursor:pointer;color:#2563eb}details form{display:grid;gap:7px;max-width:420px;margin-top:10px}.inline{display:inline}code{background:#f1f5f9;padding:2px 5px;border-radius:5px}.buttons{display:flex;gap:8px;flex-wrap:wrap}.warn{color:#b45309}@media(max-width:850px){.grid{grid-template-columns:1fr}.panel{padding:13px}td{min-width:120px}}
</style></head><body><div class="wrap"><h1>Linux.do 回复监控</h1><p class="sub">Go + SQLite · 多规则监控 · SMTP 持久化重试 · 默认只提醒新增回复</p>
{{if .Message}}<p class="notice">{{.Message}}</p>{{end}}
<div class="panel"><h2>添加监控规则</h2><form action="/rules" method="post" class="grid"><input type="hidden" name="csrf" value="{{.CSRF}}"><div><label>主题地址（末尾楼层号会自动忽略）</label><input name="topic_url" type="url" required placeholder="https://linux.do/t/topic/2696141/33528"></div><div><label>目标用户名</label><input name="username" required placeholder="Andrew-hh"></div><button type="submit">添加监控</button></form><p class="muted">规则创建后首次成功检查将自动建立当前基线，不发送历史回复。修改主题或用户名会重新建基线。</p></div>
<div class="panel"><h2>监控规则（{{len .Rules}}）</h2><div class="scroll"><table><thead><tr><th>主题 / 用户</th><th>状态</th><th>游标</th><th>邮件</th><th>检查状态</th><th>操作</th></tr></thead><tbody>
{{range .Rules}}<tr><td><strong><a href="{{.TopicURL}}" target="_blank" rel="noopener noreferrer">{{if .Title}}{{.Title}}{{else}}主题 {{.TopicID}}{{end}}</a></strong><br><code>@{{.Username}}</code><br><small>ID {{.TopicID}}</small></td><td>{{if .Enabled}}<span class="tag green">监控中</span>{{else}}<span class="tag">已暂停</span>{{end}}{{if not .Initialized}}<p class="warn">尚未初始化</p>{{end}}</td><td>楼层：{{.CursorNumber}}<br><small>Post ID：{{.CursorID}}</small></td><td>待发 {{.PendingCount}}<br>已发 {{.SentCount}}</td><td><small>最近：{{when .LastChecked}}<br>下次：{{when .NextCheck}}</small>{{if .LastError}}<p class="bad">{{short .LastError}}</p>{{end}}</td><td><div class="actions">
<form method="post" action="/rules/{{.ID}}/check"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="small green">立即检查</button></form>
<form method="post" action="/rules/{{.ID}}/toggle"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="small gray">{{if .Enabled}}暂停{{else}}启用{{end}}</button></form>
<form method="post" action="/rules/{{.ID}}/delete" onsubmit="return confirm('确定删除监控规则及历史通知记录？')"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button class="small red">删除</button></form></div>
<details><summary>修改规则</summary><form method="post" action="/rules/{{.ID}}/edit"><input type="hidden" name="csrf" value="{{$.CSRF}}"><label>主题地址</label><input type="url" name="topic_url" value="{{.TopicURL}}" required><label>用户名</label><input name="username" value="{{.Username}}" required><button class="small">保存修改</button></form></details></td></tr>{{else}}<tr><td colspan="6" class="muted">暂无规则。请在上方添加主题和用户名。</td></tr>{{end}}
</tbody></table></div></div>
<div class="panel"><h2>连接测试</h2><div class="buttons"><form method="post" action="/test-mail" class="inline"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="gray">发送 SMTP 测试邮件</button></form><form method="post" action="/test-source" class="inline"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="url" name="topic_url" value="https://linux.do/t/topic/2696141" required style="width:260px;max-width:100%;margin-right:6px"><button class="gray">测试主题接口</button></form></div><p class="muted">收件邮箱：{{.Config.SMTPTo}} · 检查间隔：{{.Config.PollInterval}} · 远端 403/429 会在状态栏显示并自动退避重试。</p></div>
<div class="panel"><h2>最近邮件（最多 30 条）</h2><div class="scroll"><table><thead><tr><th>楼层直达链接</th><th>作者</th><th>状态</th><th>重试次数 / 错误</th></tr></thead><tbody>{{range .Deliveries}}<tr><td><a href="{{.PostURL}}" target="_blank" rel="noopener noreferrer">{{.TopicTitle}} #{{.PostNumber}}</a></td><td>@{{.Username}}</td><td>{{if eq .Status "sent"}}<span class="ok">已提交到 SMTP</span>{{else}}<span class="warn">待发送</span>{{end}}</td><td>{{.Attempts}} {{if .LastError}}<br><small class="bad">{{short .LastError}}</small>{{end}}</td></tr>{{else}}<tr><td colspan="4" class="muted">暂无邮件记录</td></tr>{{end}}</tbody></table></div></div>
<p class="muted">安全建议：管理界面仅监听 127.0.0.1，由 HTTPS 反向代理提供外网访问。敏感密码只保存在服务器环境变量，不显示在页面中。</p></div></body></html>`))

func newWebApp(db *sql.DB, c Config, d *DiscourseClient) *WebApp {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return &WebApp{DB: db, Config: c, Discourse: d, CSRF: base64.RawURLEncoding.EncodeToString(buf)}
}
func (a *WebApp) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pwd, ok := r.BasicAuth()
		goodUser := subtle.ConstantTimeCompare([]byte(user), []byte(a.Config.AdminUser)) == 1
		goodPass := subtle.ConstantTimeCompare([]byte(pwd), []byte(a.Config.AdminPassword)) == 1
		if !ok || !goodUser || !goodPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Linux.do Monitor", charset="UTF-8"`)
			http.Error(w, "需要管理员登录", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "无效表单", 400)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(a.CSRF)) != 1 {
				http.Error(w, "CSRF 校验失败，请刷新页面", 403)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, e := url.Parse(origin)
				if e != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "跨站请求已拒绝", 403)
					return
				}
			}
		}
		next(w, r)
	}
}
func (a *WebApp) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /", a.auth(a.index))
	mux.HandleFunc("POST /rules", a.auth(a.add))
	mux.HandleFunc("POST /rules/{id}/{action}", a.auth(a.ruleAction))
	mux.HandleFunc("POST /test-mail", a.auth(a.testMail))
	mux.HandleFunc("POST /test-source", a.auth(a.testSource))
	return mux
}
func (a *WebApp) message(w http.ResponseWriter, r *http.Request, s string) {
	http.Redirect(w, r, "/?msg="+url.QueryEscape(s), http.StatusSeeOther)
}
func (a *WebApp) index(w http.ResponseWriter, r *http.Request) {
	rules, err := listRules(a.DB)
	if err != nil {
		http.Error(w, "读取规则失败", 500)
		log.Print(err)
		return
	}
	mails, err := recentDeliveries(a.DB)
	if err != nil {
		http.Error(w, "读取邮件状态失败", 500)
		log.Print(err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, pageData{Rules: rules, Deliveries: mails, Config: a.Config, CSRF: a.CSRF, Message: r.URL.Query().Get("msg")}); err != nil {
		log.Printf("渲染 UI: %v", err)
	}
}
func formRule(r *http.Request) (int64, string, string, error) {
	id, u, err := parseTopicURL(r.PostFormValue("topic_url"))
	if err != nil {
		return 0, "", "", err
	}
	name, err := normalizedUsername(r.PostFormValue("username"))
	return id, u, name, err
}
func (a *WebApp) add(w http.ResponseWriter, r *http.Request) {
	id, u, name, err := formRule(r)
	if err == nil {
		err = addRule(a.DB, id, u, name)
	}
	if err != nil {
		a.message(w, r, "新增失败："+err.Error())
		return
	}
	a.message(w, r, "规则已添加：首次成功检查自动建立当前游标，不发送历史帖子")
}
func (a *WebApp) ruleAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "规则 ID 错误", 400)
		return
	}
	switch r.PathValue("action") {
	case "edit":
		var topicID int64
		var u, name string
		topicID, u, name, err = formRule(r)
		if err == nil {
			err = editRule(a.DB, id, topicID, u, name)
		}
	case "toggle":
		err = toggleRule(a.DB, id)
	case "delete":
		err = deleteRule(a.DB, id)
	case "check":
		err = checkNow(a.DB, id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.message(w, r, "操作失败："+err.Error())
	} else {
		a.message(w, r, "操作已保存")
	}
}
func (a *WebApp) testMail(w http.ResponseWriter, r *http.Request) {
	err := sendTestMail(a.Config)
	if err != nil {
		a.message(w, r, "SMTP 测试失败："+err.Error())
	} else {
		a.message(w, r, "SMTP 测试邮件已提交（请查看邮箱）")
	}
}
func (a *WebApp) testSource(w http.ResponseWriter, r *http.Request) {
	id, _, err := parseTopicURL(r.PostFormValue("topic_url"))
	if err != nil {
		a.message(w, r, err.Error())
		return
	}
	t, err := a.Discourse.Topic(r.Context(), id)
	if err != nil {
		a.message(w, r, "接口测试失败："+err.Error())
		return
	}
	mode := "普通主题 / ID 流"
	if t.Stream.IsMegaTopic || t.Stream.IDs == nil {
		mode = "超长主题 / 楼层窗口"
	}
	a.message(w, r, fmt.Sprintf("接口可用：%s，最高楼层 %d，模式 %s", t.Title, t.LatestNumber(), mode))
}
