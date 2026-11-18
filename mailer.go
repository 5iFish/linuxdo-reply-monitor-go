package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	stdhtml "html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	xhtml "golang.org/x/net/html"
)

var emailHTMLPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Discourse 的上传图片、主题引用经常使用相对链接；仅在安全清洗后统一补全地址。
	p.AllowRelativeURLs(true)
	return p
}()

func safeHTML(raw string) string {
	clean := emailHTMLPolicy.Sanitize(raw)
	root, err := xhtml.Parse(strings.NewReader(clean))
	if err != nil {
		return clean
	}
	base := &url.URL{Scheme: "https", Host: "linux.do", Path: "/"}
	var visit func(*xhtml.Node)
	visit = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for i, a := range n.Attr {
				if a.Key == "href" || a.Key == "src" {
					val := strings.TrimSpace(a.Val)
					parsed, err := url.Parse(val)
					if err != nil || parsed.Scheme != "" {
						continue
					}
					if parsed.Host != "" {
						parsed.Scheme = "https" // 将协议相对链接固定为 HTTPS
						n.Attr[i].Val = parsed.String()
					} else {
						n.Attr[i].Val = base.ResolveReference(parsed).String()
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(root)
	var body *xhtml.Node
	var find func(*xhtml.Node)
	find = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if body == nil {
				find(c)
			}
		}
	}
	find(root)
	if body == nil {
		return clean
	}
	var out bytes.Buffer
	for node := body.FirstChild; node != nil; node = node.NextSibling {
		_ = xhtml.Render(&out, node)
	}
	return out.String()
}
func plainFromHTML(raw string) string {
	root, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return stdhtml.UnescapeString(raw)
	}
	var out strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			out.WriteString(n.Data)
		}
		if n.Type == xhtml.ElementNode {
			if n.Data == "br" || n.Data == "p" || n.Data == "li" || n.Data == "blockquote" || n.Data == "pre" {
				out.WriteByte('\n')
			}
			if n.Data == "img" {
				for _, a := range n.Attr {
					if a.Key == "src" {
						out.WriteString("[图片: " + a.Val + "]")
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == xhtml.ElementNode && (n.Data == "p" || n.Data == "div" || n.Data == "li" || n.Data == "blockquote") {
			out.WriteByte('\n')
		}
	}
	walk(root)
	return strings.TrimSpace(out.String())
}
func addPart(w *multipart.Writer, typ, content string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", typ+"; charset=UTF-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	q := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(q, content); err != nil {
		return err
	}
	return q.Close()
}
func sendRaw(c Config, subject, textBody, htmlBody, messageID string) error {
	from, err := mail.ParseAddress(c.SMTPFrom)
	if err != nil {
		return err
	}
	recipients, err := mail.ParseAddressList(c.SMTPTo)
	if err != nil {
		return err
	}
	tos := make([]string, 0, len(recipients))
	labels := make([]string, 0, len(recipients))
	for _, to := range recipients {
		tos = append(tos, to.Address)
		labels = append(labels, to.String())
	}
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "From: %s\r\n", from.String())
	fmt.Fprintf(&raw, "To: %s\r\n", strings.Join(labels, ", "))
	fmt.Fprintf(&raw, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", truncate(subject, 180)))
	fmt.Fprintf(&raw, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	if messageID != "" {
		fmt.Fprintf(&raw, "Message-ID: <%s>\r\n", messageID)
	}
	raw.WriteString("MIME-Version: 1.0\r\n")
	writer := multipart.NewWriter(&raw)
	fmt.Fprintf(&raw, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", writer.Boundary())
	if err := addPart(writer, "text/plain", textBody); err != nil {
		return err
	}
	if err := addPart(writer, "text/html", htmlBody); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return smtpSend(c, from.Address, tos, raw.Bytes())
}
func smtpSend(c Config, from string, tos []string, message []byte) error {
	address := net.JoinHostPort(c.SMTPHost, c.SMTPPort)
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	if c.SMTPTLSMode == "implicit" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", address)
	}
	if err != nil {
		return fmt.Errorf("SMTP 连接失败: %w", err)
	}
	// 限制整个 SMTP 会话（包括认证、DATA 和 QUIT）的耗时，避免卡住工作线程。
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
	client, err := smtp.NewClient(conn, c.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if c.SMTPTLSMode == "starttls" {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("SMTP 服务器不支持 STARTTLS，拒绝明文发送")
		}
		if err := client.StartTLS(&tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if c.SMTPUser != "" {
		if err := client.Auth(smtp.PlainAuth("", c.SMTPUser, c.SMTPPassword, c.SMTPHost)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, to := range tos {
		if err := client.Rcpt(to); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(message); err != nil {
		writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	// DATA 已收到成功响应，意味着 SMTP 服务端接受邮件。
	// 即使后续 QUIT 断线，也不能因为 QUIT 失败而重发同一条邮件。
	_ = client.Quit()
	return nil
}
func sendDelivery(c Config, d Delivery) error {
	content := safeHTML(d.Cooked)
	title := d.TopicTitle
	if title == "" {
		title = "Linux.do 主题"
	}
	subject := fmt.Sprintf("[Linux.do 回复] @%s | %s (#%d)", d.Username, title, d.PostNumber)
	plain := fmt.Sprintf("主题：%s\n作者：@%s\n时间：%s\n楼层：#%d\n原帖：%s\n\n%s\n", title, d.Username, d.CreatedAt, d.PostNumber, d.PostURL, plainFromHTML(content))
	htmlBody := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body style="font-family:Arial,sans-serif;line-height:1.7;color:#222"><h2>%s</h2><p>作者：<strong>@%s</strong><br>发布时间：%s<br>楼层：#%d</p><hr><article>%s</article><hr><p><a href="%s">点击查看原帖第 %d 楼</a></p></body></html>`, stdhtml.EscapeString(title), stdhtml.EscapeString(d.Username), stdhtml.EscapeString(d.CreatedAt), d.PostNumber, content, stdhtml.EscapeString(d.PostURL), d.PostNumber)
	from, err := mail.ParseAddress(c.SMTPFrom)
	if err != nil {
		return err
	}
	at := strings.LastIndex(from.Address, "@")
	if at < 0 {
		return fmt.Errorf("SMTP_FROM 缺少邮箱域名")
	}
	return sendRaw(c, subject, plain, htmlBody, fmt.Sprintf("linuxdo-%d-%d@%s", d.RuleID, d.PostID, from.Address[at+1:]))
}
func sendTestMail(c Config) error {
	subject := "Linux.do 回复监控：SMTP 测试邮件"
	plain := "测试成功：Linux.do 监控程序已连接你的 SMTP 服务器。"
	body := "<p>测试成功：Linux.do 监控程序已连接你的 SMTP 服务器。</p>"
	return sendRaw(c, subject, plain, body, "")
}
