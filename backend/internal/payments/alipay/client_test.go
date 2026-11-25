package alipay

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"os"
	"testing"

	"github.com/jucode/jucode/internal/conf"
)

func TestAESContentRoundTrip(t *testing.T) {
	const key = "MTIzNDU2Nzg5MGFiY2RlZg=="
	plain := `{"out_trade_no":"SO1","total_amount":"1.00"}`
	encrypted, err := encryptContent(plain, key)
	if err != nil {
		t.Fatalf("encryptContent: %v", err)
	}
	decrypted, err := decryptContent(encrypted, key)
	if err != nil {
		t.Fatalf("decryptContent: %v", err)
	}
	if decrypted != plain {
		t.Fatalf("decrypted mismatch: got %q want %q", decrypted, plain)
	}
}

func TestDecryptResponseBody(t *testing.T) {
	const key = "MTIzNDU2Nzg5MGFiY2RlZg=="
	plain := `{"code":"10000","trade_status":"TRADE_SUCCESS"}`
	encrypted, err := encryptContent(plain, key)
	if err != nil {
		t.Fatalf("encryptContent: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"alipay_trade_query_response": encrypted,
		"sign":                        "stub",
	})
	decrypted, err := decryptResponseBody("alipay.trade.query", body, key)
	if err != nil {
		t.Fatalf("decryptResponseBody: %v", err)
	}
	var out queryResponse
	if err := json.Unmarshal(decrypted, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out.Response.Code != "10000" || out.Response.TradeStatus != "TRADE_SUCCESS" {
		t.Fatalf("unexpected response: %+v", out.Response)
	}
}

// 电脑网站支付和手机网站支付是两个产品：前者只能扫码，后者才会唤起支付宝 App。
// 一旦 method 或 product_code 串了，手机端就退化成扫码页，所以两条链路都钉住。
func TestPrepayProductCodes(t *testing.T) {
	c := newTestClient(t)
	in := NativePrepayInput{
		OrderNo:     "SO123",
		Description: "余额充值",
		AmountCents: 123,
		ReturnURL:   "https://console.example.com/me/billing",
		QuitURL:     "https://console.example.com/me/billing",
	}

	page, err := c.NativePrepay(t.Context(), in)
	if err != nil {
		t.Fatalf("NativePrepay: %v", err)
	}
	method, biz := parsePayURL(t, page.CodeURL)
	if method != "alipay.trade.page.pay" {
		t.Fatalf("page method = %q", method)
	}
	if biz["product_code"] != "FAST_INSTANT_TRADE_PAY" {
		t.Fatalf("page product_code = %q", biz["product_code"])
	}
	if _, ok := biz["quit_url"]; ok {
		t.Fatalf("quit_url must not leak into 电脑网站支付: %q", biz["quit_url"])
	}

	wap, err := c.WapPrepay(t.Context(), in)
	if err != nil {
		t.Fatalf("WapPrepay: %v", err)
	}
	method, biz = parsePayURL(t, wap.CodeURL)
	if method != "alipay.trade.wap.pay" {
		t.Fatalf("wap method = %q", method)
	}
	if biz["product_code"] != "QUICK_WAP_WAY" {
		t.Fatalf("wap product_code = %q", biz["product_code"])
	}
	if biz["quit_url"] != in.QuitURL {
		t.Fatalf("wap quit_url = %q", biz["quit_url"])
	}
	if page.CodeURL == wap.CodeURL {
		t.Fatal("两个产品必须生成不同链接")
	}
}

func parsePayURL(t *testing.T, raw string) (string, map[string]string) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	biz := map[string]string{}
	if err := json.Unmarshal([]byte(parsed.Query().Get("biz_content")), &biz); err != nil {
		t.Fatalf("biz_content json: %v", err)
	}
	return parsed.Query().Get("method"), biz
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	c, err := New(conf.AlipayConfig{
		Enabled:             true,
		AppID:               "2021000000000000",
		GatewayURL:          "https://openapi.alipay.com/gateway.do",
		PrivateKeyFile:      writeTempFile(t, privPEM),
		AlipayPublicKeyFile: writeTempFile(t, pubPEM),
		NotifyURL:           "https://example.com/v1/payments/alipay/notify",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPagePayURL(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	privFile := writeTempFile(t, privPEM)
	pubFile := writeTempFile(t, pubPEM)
	c, err := New(conf.AlipayConfig{
		Enabled:             true,
		AppID:               "2021000000000000",
		GatewayURL:          "https://openapi.alipay.com/gateway.do",
		PrivateKeyFile:      privFile,
		AlipayPublicKeyFile: pubFile,
		NotifyURL:           "https://example.com/v1/payments/alipay/notify",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u, err := c.pagePayURL("alipay.trade.page.pay", c.cfg.NotifyURL, "https://console.example.com/me/billing", map[string]any{
		"out_trade_no": "SO123",
		"total_amount": "1.23",
		"subject":      "Example 余额充值",
		"product_code": "FAST_INSTANT_TRADE_PAY",
	})
	if err != nil {
		t.Fatalf("pagePayURL: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	q := parsed.Query()
	if got := q.Get("method"); got != "alipay.trade.page.pay" {
		t.Fatalf("method = %q", got)
	}
	if got := q.Get("product_code"); got != "" {
		t.Fatalf("unexpected top-level product_code = %q", got)
	}
	if got := q.Get("sign_type"); got != "RSA2" {
		t.Fatalf("sign_type = %q", got)
	}
	if got := q.Get("notify_url"); got != c.cfg.NotifyURL {
		t.Fatalf("notify_url = %q", got)
	}
	if got := q.Get("return_url"); got != "https://console.example.com/me/billing" {
		t.Fatalf("return_url = %q", got)
	}
	if got := q.Get("biz_content"); got == "" {
		t.Fatalf("biz_content empty")
	}
	var biz map[string]string
	if err := json.Unmarshal([]byte(q.Get("biz_content")), &biz); err != nil {
		t.Fatalf("biz_content json: %v", err)
	}
	if got := biz["product_code"]; got != "FAST_INSTANT_TRADE_PAY" {
		t.Fatalf("product_code = %q", got)
	}
}

func writeTempFile(t *testing.T, b []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", "alipay-*.pem")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}
