// Package biz — email outbound service.
//
// EmailSvc is the single dial-out path for transactional email
// (verification, password reset, ticket notifications). Configuration lives in
// site_settings (loaded fresh per send so admin SMTP edits take
// effect without a service restart). When SMTPHost is empty all
// Send* calls return ErrEmailDisabled — callers should fall back to
// "email feature unavailable" UX.
//
// Why this lives in biz instead of pkg: it depends on SettingsSvc,
// and biz already houses other DB-backed services.
package biz

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/smtp"
	"strings"
)

var (
	ErrEmailDisabled      = errors.New("email: SMTP not configured")
	ErrEmailMisconfigured = errors.New("email: SMTP misconfigured")
)

type EmailSvc struct {
	settings *SettingsSvc
	logger   *slog.Logger
}

func NewEmailSvc(settings *SettingsSvc, logger *slog.Logger) *EmailSvc {
	if logger == nil {
		logger = slog.Default()
	}
	return &EmailSvc{settings: settings, logger: logger.With("svc", "email")}
}

// Enabled returns true when SMTP host is configured. Callers can
// short-circuit on this without a full send to decide whether
// optional flows (e.g. password reset) should be exposed.
func (s *EmailSvc) Enabled(ctx context.Context) bool {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return false
	}
	return strings.TrimSpace(cfg.SMTPHost) != ""
}

// Send delivers one message. Always wraps the body in a text/html
// MIME envelope and a UTF-8 charset header so non-ASCII subjects
// render correctly. Errors include the SMTP response.
func (s *EmailSvc) Send(ctx context.Context, to, subject, htmlBody string) error {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return fmt.Errorf("load smtp config: %w", err)
	}
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		return ErrEmailDisabled
	}
	if cfg.SMTPPort == 0 {
		return fmt.Errorf("%w: smtp_port must be set", ErrEmailMisconfigured)
	}
	from := cfg.SMTPFromEmail
	if from == "" {
		from = cfg.SMTPUsername
	}
	if from == "" {
		return fmt.Errorf("%w: smtp_from_email or smtp_username must be set", ErrEmailMisconfigured)
	}
	password, err := s.settings.GetSMTPSecret(ctx)
	if err != nil {
		return fmt.Errorf("load smtp password: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	fromHeader := from
	if cfg.SMTPFromName != "" {
		fromHeader = fmt.Sprintf("%s <%s>", cfg.SMTPFromName, from)
	}
	msg := buildMIME(fromHeader, to, subject, htmlBody)

	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, password, cfg.SMTPHost)
	}

	if cfg.SMTPUseTLS {
		// Explicit TLS (SMTPS, typically port 465). Dial-then-Mail
		// because Go's smtp.SendMail starts over plain and STARTTLS-es
		// later — incompatible with 465.
		return s.sendImplicitTLS(addr, cfg.SMTPHost, from, to, msg, auth)
	}
	// Plain → STARTTLS handshake via smtp.SendMail (port 587 / 25).
	if err := smtp.SendMail(addr, auth, from, []string{to}, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// sendImplicitTLS dials an implicit-TLS smtps server (no STARTTLS,
// the entire conversation is encrypted from the first byte).
func (s *EmailSvc) sendImplicitTLS(addr, host, from, to string, body []byte, auth smtp.Auth) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return fmt.Errorf("smtps dial: %w", err)
	}
	defer conn.Close()
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return fmt.Errorf("smtp auth: %w", err)
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return c.Quit()
}

func buildMIME(fromHeader, to, subject, html string) []byte {
	var sb strings.Builder
	sb.WriteString("From: " + fromHeader + "\r\n")
	sb.WriteString("To: " + to + "\r\n")
	// MIME-encoded subject so UTF-8 (中文) survives transit.
	sb.WriteString("Subject: =?UTF-8?B?" + base64Encode(subject) + "?=\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(html)
	return []byte(sb.String())
}

func base64Encode(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n int
		var pad int
		switch len(b) - i {
		case 1:
			n = int(b[i]) << 16
			pad = 2
		case 2:
			n = int(b[i])<<16 | int(b[i+1])<<8
			pad = 1
		default:
			n = int(b[i])<<16 | int(b[i+1])<<8 | int(b[i+2])
		}
		out.WriteByte(tbl[(n>>18)&0x3f])
		out.WriteByte(tbl[(n>>12)&0x3f])
		if pad < 2 {
			out.WriteByte(tbl[(n>>6)&0x3f])
		} else {
			out.WriteByte('=')
		}
		if pad < 1 {
			out.WriteByte(tbl[n&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// --- templates ---
//
// Kept inline here as tiny strings; the moment they need real layout
// we should move to text/template files. For three transactional
// emails, inline templates are still enough.

// VerificationEmail builds the body for register / change_email kinds.
// The link should point at the public frontend route that handles the
// token exchange, e.g. https://site.example/verify-email?code=...
func VerificationEmail(siteName, link string) (subject, body string) {
	subject = "[" + siteName + "] 邮箱验证"
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">验证你的邮箱</h2>
<p>请点击下方链接验证邮箱地址。链接 24 小时内有效。</p>
<p><a href="%s" style="display: inline-block; padding: 10px 20px; background: #5B5BD6; color: #fff; text-decoration: none; border-radius: 6px;">验证邮箱</a></p>
<p style="color: #888; font-size: 12px;">如果按钮无法点击,请复制此链接到浏览器:<br>%s</p>
<p style="color: #888; font-size: 12px;">如果你没有发起这个请求,请忽略此邮件。</p>
</body></html>`, link, link)
	return
}

func RegisterCodeEmail(siteName, code string) (subject, body string) {
	subject = "[" + siteName + "] 注册验证码"
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">注册验证码</h2>
<p>请在注册页面输入以下验证码。验证码 15 分钟内有效。</p>
<p style="font-size: 28px; letter-spacing: 6px; font-weight: 700; margin: 18px 0;">%s</p>
<p style="color: #888; font-size: 12px;">如果你没有发起这个请求,请忽略此邮件。</p>
</body></html>`, code)
	return
}

func PasswordResetEmail(siteName, link string) (subject, body string) {
	subject = "[" + siteName + "] 密码重置"
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">重置你的密码</h2>
<p>请点击下方链接重置密码。链接 1 小时内有效。</p>
<p><a href="%s" style="display: inline-block; padding: 10px 20px; background: #5B5BD6; color: #fff; text-decoration: none; border-radius: 6px;">重置密码</a></p>
<p style="color: #888; font-size: 12px;">如果按钮无法点击,请复制此链接到浏览器:<br>%s</p>
<p style="color: #888; font-size: 12px;">如果你没有发起这个请求,请忽略此邮件。你的密码不会被修改。</p>
</body></html>`, link, link)
	return
}

func TicketAssignedEmail(siteName, title, link string) (subject, body string) {
	subject = "[" + siteName + "] 新工单指派"
	safeTitle := html.EscapeString(title)
	safeLink := html.EscapeString(link)
	action := `<p>请登录客服后台查看并处理该工单。</p>`
	if link != "" {
		action = fmt.Sprintf(`<p><a href="%s" style="display: inline-block; padding: 10px 20px; background: #5B5BD6; color: #fff; text-decoration: none; border-radius: 6px;">查看工单队列</a></p>
<p style="color: #888; font-size: 12px;">如果按钮无法点击,请复制此链接到浏览器:<br>%s</p>`, safeLink, safeLink)
	}
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">你有一个新指派的工单</h2>
<p>系统已将以下工单指派给你:</p>
<p style="font-size: 16px; font-weight: 700; margin: 18px 0;">%s</p>
%s
</body></html>`, safeTitle, action)
	return
}

func TicketReplyEmail(siteName, title, link string, staffRecipient bool) (subject, body string) {
	subject = "[" + siteName + "] 工单有新回复"
	safeTitle := html.EscapeString(title)
	safeLink := html.EscapeString(link)
	heading := "你的工单有新回复"
	intro := "客服已回复以下工单:"
	button := "查看工单"
	fallback := "请登录用户中心查看该工单。"
	if staffRecipient {
		heading = "指派给你的工单有新回复"
		intro = "用户已回复以下工单:"
		button = "查看工单队列"
		fallback = "请登录客服后台查看并处理该工单。"
	}
	action := fmt.Sprintf(`<p>%s</p>`, fallback)
	if link != "" {
		action = fmt.Sprintf(`<p><a href="%s" style="display: inline-block; padding: 10px 20px; background: #5B5BD6; color: #fff; text-decoration: none; border-radius: 6px;">%s</a></p>
<p style="color: #888; font-size: 12px;">如果按钮无法点击,请复制此链接到浏览器:<br>%s</p>`, safeLink, button, safeLink)
	}
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">%s</h2>
<p>%s</p>
<p style="font-size: 16px; font-weight: 700; margin: 18px 0;">%s</p>
%s
</body></html>`, heading, intro, safeTitle, action)
	return
}

func TicketStatusEmail(siteName, title, status, link string) (subject, body string) {
	subject = "[" + siteName + "] 工单状态更新"
	safeTitle := html.EscapeString(title)
	safeStatus := html.EscapeString(ticketStatusLabel(status))
	safeLink := html.EscapeString(link)
	action := `<p>请登录用户中心查看该工单。</p>`
	if link != "" {
		action = fmt.Sprintf(`<p><a href="%s" style="display: inline-block; padding: 10px 20px; background: #5B5BD6; color: #fff; text-decoration: none; border-radius: 6px;">查看工单</a></p>
<p style="color: #888; font-size: 12px;">如果按钮无法点击,请复制此链接到浏览器:<br>%s</p>`, safeLink, safeLink)
	}
	body = fmt.Sprintf(`<!doctype html><html><body style="font-family: -apple-system, sans-serif; max-width: 560px; margin: 0 auto; padding: 24px;">
<h2 style="margin: 0 0 12px;">工单状态已更新</h2>
<p>以下工单状态已更新为 <strong>%s</strong>:</p>
<p style="font-size: 16px; font-weight: 700; margin: 18px 0;">%s</p>
%s
</body></html>`, safeStatus, safeTitle, action)
	return
}

func ticketStatusLabel(status string) string {
	switch status {
	case "open":
		return "待处理"
	case "in_progress":
		return "处理中"
	case "waiting_user":
		return "等待回复"
	case "resolved":
		return "已解决"
	case "closed":
		return "已关闭"
	default:
		return status
	}
}
