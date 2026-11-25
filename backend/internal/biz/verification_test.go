package biz

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	entevt "github.com/jucode/jucode/internal/data/ent/emailverificationtoken"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
)

// Receives only local test mail; no external SMTP service is used.
func verificationSMTP(t *testing.T) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	messages := make(chan string, 10)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				writer := bufio.NewWriter(conn)
				reply := func(s string) { _, _ = writer.WriteString(s + "\r\n"); _ = writer.Flush() }
				reply("220 localhost SMTP")
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					switch {
					case scanner.Text() == "DATA":
						reply("354 send data")
						var body strings.Builder
						for scanner.Scan() && scanner.Text() != "." {
							body.WriteString(scanner.Text() + "\n")
						}
						messages <- body.String()
						reply("250 accepted")
					case scanner.Text() == "QUIT":
						reply("221 bye")
						return
					default:
						reply("250 localhost")
					}
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, messages
}

func TestExistingUserEmailVerification(t *testing.T) {
	invites, settings, client := testInviteEnv(t)
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	invites.d.Redis = rdb
	port, messages := verificationSMTP(t)
	cfg, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUseTLS = "127.0.0.1", port, false
	cfg.SMTPUsername, cfg.SMTPFromEmail = "", "sender@test.invalid"
	cfg.SiteURL, cfg.RegistrationEnabled, cfg.RequireRegisterVerification = "https://example.com", false, false
	settings.storeSite(cfg)
	cipher, err := cryptopkg.New(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	settings.SetCipher(cipher)
	verify := NewVerificationSvc(invites.d, settings, NewEmailSvc(settings, nil))
	acc := NewAccount(invites.d, nil, settings)
	acc.SetVerification(verify)
	user := newInviteTestUser(t, client)
	other := newInviteTestUser(t, client)
	if err := acc.RequestEmailVerification(ctx, user, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	mail := <-messages
	if !strings.Contains(mail, "To: "+user.Email) {
		t.Fatalf("mail must target the current user's address: %s", mail)
	}
	link := regexp.MustCompile(`https://example.com/verify-email\?[^"\s]+`).FindString(mail)
	parsed, err := url.Parse(link)
	if err != nil || parsed.Query().Get("kind") != "register" {
		t.Fatalf("verification link = %q, err = %v", link, err)
	}
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatal("missing verification code")
	}
	if err := acc.RequestEmailVerification(ctx, user, ""); !errors.Is(err, ErrVerifyCodeTooFrequent) {
		t.Fatalf("second send = %v, want cooldown", err)
	}
	if _, err := verify.Consume(ctx, code, entevt.KindChangeEmail); !errors.Is(err, ErrVerificationNotFound) {
		t.Fatalf("wrong token kind = %v", err)
	}
	verified, err := acc.ConsumeRegisterVerification(ctx, code)
	if err != nil || !verified.EmailVerified || verified.EmailVerifiedAt == nil || verified.Email != user.Email {
		t.Fatalf("verification result = %+v, err = %v", verified, err)
	}
	if client.User.GetX(ctx, other.ID).EmailVerified {
		t.Fatal("verification changed another user")
	}
	if _, err := acc.ConsumeRegisterVerification(ctx, code); !errors.Is(err, ErrVerificationUsed) {
		t.Fatalf("replay = %v", err)
	}
	if err := acc.RequestEmailVerification(ctx, verified, ""); !errors.Is(err, ErrEmailAlreadyVerified) {
		t.Fatalf("verified user resend = %v", err)
	}
	// A new request invalidates the old link, and a subsequent email change
	// prevents the replacement link from verifying an unproven address.
	if err := acc.RequestEmailVerification(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	<-messages
	old := client.EmailVerificationToken.Query().Where(entevt.UserIDEQ(other.ID)).OnlyX(ctx)
	mr.FastForward(time.Minute)
	if err := acc.RequestEmailVerification(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	<-messages
	if _, err := acc.ConsumeRegisterVerification(ctx, old.Code); !errors.Is(err, ErrVerificationUsed) {
		t.Fatalf("replaced token = %v", err)
	}
	replacement := client.EmailVerificationToken.Query().Where(entevt.UserIDEQ(other.ID), entevt.UsedAtIsNil()).OnlyX(ctx)
	client.User.UpdateOneID(other.ID).SetEmail("changed-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@test.invalid").ExecX(ctx)
	if _, err := acc.ConsumeRegisterVerification(ctx, replacement.Code); !errors.Is(err, ErrVerificationNotFound) {
		t.Fatalf("link for prior address = %v", err)
	}
	if client.User.GetX(ctx, other.ID).EmailVerified {
		t.Fatal("old email link verified the new address")
	}
}

func TestEmailVerificationFailureCanRetry(t *testing.T) {
	invites, settings, client := testInviteEnv(t)
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	invites.d.Redis = rdb
	cfg, _ := settings.GetSite(ctx)
	port, _ := verificationSMTP(t)
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPFromEmail = "127.0.0.1", port, "sender@test.invalid"
	settings.storeSite(cfg)
	// Missing cipher makes the SMTP configuration fail before delivery.
	verify := NewVerificationSvc(invites.d, settings, NewEmailSvc(settings, nil))
	u := newInviteTestUser(t, client)
	for range 2 {
		err := verify.IssueRegister(ctx, u, "")
		if err == nil || errors.Is(err, ErrVerifyCodeTooFrequent) {
			t.Fatalf("failed delivery must allow immediate retry: %v", err)
		}
	}
}
