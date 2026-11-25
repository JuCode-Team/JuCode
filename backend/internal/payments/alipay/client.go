package alipay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/conf"
)

const defaultGatewayURL = "https://openapi.alipay.com/gateway.do"

var (
	ErrDisabled        = errors.New("alipay disabled")
	ErrInvalidCallback = errors.New("invalid alipay callback")
)

type Client struct {
	cfg        conf.AlipayConfig
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	http       *http.Client
}

type NativePrepayInput struct {
	OrderNo     string
	Description string
	AmountCents int64
	Attach      string
	ReturnURL   string
	// QuitURL 只对手机网站支付生效：H5 收银台上的返回按钮跳这里。
	// 唤起支付宝 App 成功时该按钮不出现，属于文档写明的行为。
	QuitURL string
}

type NativePrepayResult struct {
	CodeURL     string
	AmountCents int64
}

type QueryResult struct {
	TradeState    string
	TransactionID string
	PayerOpenID   string
	AmountCents   int64
}

type CallbackResult struct {
	OrderNo       string
	TradeState    string
	TransactionID string
	PayerOpenID   string
	AmountCents   int64
}

func New(cfg conf.AlipayConfig) (*Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	cfg.GatewayURL = strings.TrimSpace(cfg.GatewayURL)
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = defaultGatewayURL
	}
	missing := missingConfig(cfg)
	if len(missing) > 0 {
		return nil, fmt.Errorf("alipay missing config: %s", strings.Join(missing, ", "))
	}
	priv, err := loadPrivateKey(cfg.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load alipay private key: %w", err)
	}
	pub, err := loadPublicKey(cfg.AlipayPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load alipay public key: %w", err)
	}
	return &Client{
		cfg:        cfg,
		privateKey: priv,
		publicKey:  pub,
		http:       &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func missingConfig(cfg conf.AlipayConfig) []string {
	var out []string
	check := []struct {
		name string
		val  string
	}{
		{"appid", cfg.AppID},
		{"private_key_file", cfg.PrivateKeyFile},
		{"alipay_public_key_file", cfg.AlipayPublicKeyFile},
		{"notify_url", cfg.NotifyURL},
	}
	for _, it := range check {
		if strings.TrimSpace(it.val) == "" {
			out = append(out, it.name)
		}
	}
	return out
}

func (c *Client) Enabled() bool { return c != nil }

// NativePrepay 走电脑网站支付，返回的是 PC 收银台地址（扫码或登录付款）。
// 该产品没有唤起支付宝 App 的能力，手机浏览器请改用 WapPrepay。
func (c *Client) NativePrepay(ctx context.Context, in NativePrepayInput) (*NativePrepayResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	bizContent, err := c.tradeBizContent(in, "FAST_INSTANT_TRADE_PAY")
	if err != nil {
		return nil, err
	}
	payURL, err := c.pagePayURL("alipay.trade.page.pay", c.cfg.NotifyURL, in.ReturnURL, bizContent)
	if err != nil {
		return nil, err
	}
	return &NativePrepayResult{CodeURL: payURL, AmountCents: in.AmountCents}, nil
}

// WapPrepay 走手机网站支付。装了支付宝 App 的手机浏览器打开返回的地址会自动
// 唤起 App，唤不起则自动降级到 H5 收银台，两条路都不需要前端拼 alipays:// scheme。
// 需要在开放平台单独签约「手机网站支付」，未签约时网关返回 40006。
func (c *Client) WapPrepay(ctx context.Context, in NativePrepayInput) (*NativePrepayResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	bizContent, err := c.tradeBizContent(in, "QUICK_WAP_WAY")
	if err != nil {
		return nil, err
	}
	if quitURL := strings.TrimSpace(in.QuitURL); quitURL != "" {
		bizContent["quit_url"] = quitURL
	}
	payURL, err := c.pagePayURL("alipay.trade.wap.pay", c.cfg.NotifyURL, in.ReturnURL, bizContent)
	if err != nil {
		return nil, err
	}
	return &NativePrepayResult{CodeURL: payURL, AmountCents: in.AmountCents}, nil
}

func (c *Client) tradeBizContent(in NativePrepayInput, productCode string) (map[string]any, error) {
	if in.AmountCents <= 0 {
		return nil, errors.New("alipay amount cents must be positive")
	}
	bizContent := map[string]any{
		"out_trade_no": in.OrderNo,
		"total_amount": centsToYuan(in.AmountCents),
		"subject":      c.description(in.Description),
		"product_code": productCode,
	}
	if attach := strings.TrimSpace(in.Attach); attach != "" {
		bizContent["body"] = attach
	}
	return bizContent, nil
}

func (c *Client) QueryByOutTradeNo(ctx context.Context, outTradeNo string) (*QueryResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	bizContent := map[string]any{"out_trade_no": outTradeNo}
	var out queryResponse
	if err := c.request(ctx, "alipay.trade.query", "", bizContent, &out); err != nil {
		return nil, err
	}
	resp := out.Response
	if resp.Code != "10000" {
		return nil, fmt.Errorf("alipay query failed: code=%s msg=%s sub_code=%s sub_msg=%s", resp.Code, resp.Msg, resp.SubCode, resp.SubMsg)
	}
	amountCents := int64(0)
	if strings.TrimSpace(resp.TotalAmount) != "" {
		cents, err := yuanToCents(resp.TotalAmount)
		if err != nil {
			return nil, err
		}
		amountCents = cents
	}
	payer := payerID(resp.BuyerUserID, resp.BuyerOpenID, resp.BuyerLogonID)
	return &QueryResult{
		TradeState:    resp.TradeStatus,
		TransactionID: resp.TradeNo,
		PayerOpenID:   payer,
		AmountCents:   amountCents,
	}, nil
}

func (c *Client) ParseCallback(headers http.Header, body []byte) (*CallbackResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("%w: parse form: %v", ErrInvalidCallback, err)
	}
	if vals.Get("app_id") != c.cfg.AppID {
		return nil, fmt.Errorf("%w: app_id mismatch", ErrInvalidCallback)
	}
	if err := c.verifyCallback(vals); err != nil {
		return nil, err
	}
	amountCents := int64(0)
	if strings.TrimSpace(vals.Get("total_amount")) != "" {
		amountCents, err = yuanToCents(vals.Get("total_amount"))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid total_amount: %v", ErrInvalidCallback, err)
		}
	}
	payer := payerID(vals.Get("buyer_id"), vals.Get("buyer_open_id"), vals.Get("buyer_logon_id"))
	return &CallbackResult{
		OrderNo:       vals.Get("out_trade_no"),
		TradeState:    vals.Get("trade_status"),
		TransactionID: vals.Get("trade_no"),
		PayerOpenID:   payer,
		AmountCents:   amountCents,
	}, nil
}

func (c *Client) request(ctx context.Context, method string, notifyURL string, bizContent any, out any) error {
	params, err := c.signedParams(method, notifyURL, "", bizContent)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.GatewayURL, bytes.NewBufferString(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alipay %s failed: status=%d body=%s", method, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if len(respBody) == 0 || out == nil {
		return nil
	}
	if strings.TrimSpace(c.cfg.EncryptKey) != "" {
		respBody, err = decryptResponseBody(method, respBody, c.cfg.EncryptKey)
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(respBody, out)
}

func (c *Client) pagePayURL(method string, notifyURL string, returnURL string, bizContent any) (string, error) {
	params, err := c.signedParams(method, notifyURL, returnURL, bizContent)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(c.cfg.GatewayURL, "?") + "?" + params.Encode(), nil
}

func (c *Client) signedParams(method string, notifyURL string, returnURL string, bizContent any) (url.Values, error) {
	payload, err := json.Marshal(bizContent)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("app_id", c.cfg.AppID)
	params.Set("method", method)
	params.Set("format", "JSON")
	params.Set("charset", "utf-8")
	params.Set("sign_type", "RSA2")
	params.Set("timestamp", time.Now().Format("2006-01-02 15:04:05"))
	params.Set("version", "1.0")
	if strings.TrimSpace(c.cfg.EncryptKey) != "" {
		encrypted, err := encryptContent(string(payload), c.cfg.EncryptKey)
		if err != nil {
			return nil, err
		}
		params.Set("encrypt_type", "AES")
		params.Set("biz_content", encrypted)
	} else {
		params.Set("biz_content", string(payload))
	}
	if strings.TrimSpace(notifyURL) != "" {
		params.Set("notify_url", strings.TrimSpace(notifyURL))
	}
	if strings.TrimSpace(returnURL) != "" {
		params.Set("return_url", strings.TrimSpace(returnURL))
	}
	sig, err := c.sign([]byte(signContent(params, false)))
	if err != nil {
		return nil, err
	}
	params.Set("sign", sig)
	return params, nil
}

func (c *Client) verifyCallback(vals url.Values) error {
	if vals.Get("sign") == "" || vals.Get("sign_type") == "" {
		return fmt.Errorf("%w: missing signature", ErrInvalidCallback)
	}
	if strings.ToUpper(vals.Get("sign_type")) != "RSA2" {
		return fmt.Errorf("%w: unsupported sign_type", ErrInvalidCallback)
	}
	rawSig, err := base64.StdEncoding.DecodeString(vals.Get("sign"))
	if err != nil {
		return fmt.Errorf("%w: invalid signature encoding", ErrInvalidCallback)
	}
	sum := sha256.Sum256([]byte(signContent(vals, true)))
	if err := rsa.VerifyPKCS1v15(c.publicKey, crypto.SHA256, sum[:], rawSig); err != nil {
		return fmt.Errorf("%w: verify signature: %v", ErrInvalidCallback, err)
	}
	return nil
}

func (c *Client) sign(message []byte) (string, error) {
	sum := sha256.Sum256(message)
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func signContent(vals url.Values, callback bool) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k == "sign" || (callback && k == "sign_type") {
			continue
		}
		if strings.TrimSpace(vals.Get(k)) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+vals.Get(k))
	}
	return strings.Join(parts, "&")
}

func centsToYuan(cents int64) string {
	return decimal.NewFromInt(cents).Div(decimal.NewFromInt(100)).StringFixed(2)
}

func yuanToCents(amount string) (int64, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(amount))
	if err != nil {
		return 0, err
	}
	if d.Sign() < 0 {
		return 0, errors.New("amount must not be negative")
	}
	cents := d.Mul(decimal.NewFromInt(100)).Round(0)
	if cents.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, errors.New("amount too large")
	}
	return cents.IntPart(), nil
}

func encryptContent(content, key string) (string, error) {
	keyBytes, err := decodeAESKey(key)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad([]byte(content), block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, make([]byte, block.BlockSize())).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func decryptContent(content, key string) (string, error) {
	keyBytes, err := decodeAESKey(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw)%block.BlockSize() != 0 {
		return "", errors.New("invalid encrypted content length")
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, make([]byte, block.BlockSize())).CryptBlocks(out, raw)
	out, err = pkcs7Unpad(out, block.BlockSize())
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func decryptResponseBody(method string, body []byte, key string) ([]byte, error) {
	root := strings.ReplaceAll(method, ".", "_") + "_response"
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	for _, field := range []string{root, "error_response"} {
		raw, ok := obj[field]
		if !ok {
			continue
		}
		var cipherText string
		if err := json.Unmarshal(raw, &cipherText); err != nil {
			return body, nil
		}
		plain, err := decryptContent(cipherText, key)
		if err != nil {
			return nil, err
		}
		obj[field] = json.RawMessage(plain)
		return json.Marshal(obj)
	}
	return body, nil
}

func decodeAESKey(key string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return nil, err
	}
	if len(b) != 16 && len(b) != 24 && len(b) != 32 {
		return nil, errors.New("alipay encrypt_key must decode to 16, 24, or 32 bytes")
	}
	return b, nil
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	padding := blockSize - len(b)%blockSize
	out := make([]byte, len(b)+padding)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, errors.New("invalid padding length")
	}
	padding := int(b[len(b)-1])
	if padding == 0 || padding > blockSize || padding > len(b) {
		return nil, errors.New("invalid padding")
	}
	for _, v := range b[len(b)-padding:] {
		if int(v) != padding {
			return nil, errors.New("invalid padding")
		}
	}
	return b[:len(b)-padding], nil
}

func (c *Client) description(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "站内订单"
	}
	if len([]rune(s)) <= 256 {
		return s
	}
	return string([]rune(s)[:256])
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	der, err := decodeKeyBytes(b)
	if err != nil {
		return nil, err
	}
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if rsaKey, ok := k.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(der)
}

func loadPublicKey(path string) (*rsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	der, err := decodeKeyBytes(b)
	if err != nil {
		return nil, err
	}
	if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rsaKey, ok := pub.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("public key is not RSA")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("certificate public key is not RSA")
	}
	return rsaKey, nil
}

func decodeKeyBytes(b []byte) ([]byte, error) {
	if block, _ := pem.Decode(b); block != nil {
		return block.Bytes, nil
	}
	compact := strings.Join(strings.Fields(string(b)), "")
	der, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, errors.New("invalid PEM or base64 key")
	}
	return der, nil
}

type queryResponse struct {
	Response struct {
		Code         string `json:"code"`
		Msg          string `json:"msg"`
		SubCode      string `json:"sub_code"`
		SubMsg       string `json:"sub_msg"`
		TradeNo      string `json:"trade_no"`
		OutTradeNo   string `json:"out_trade_no"`
		TradeStatus  string `json:"trade_status"`
		TotalAmount  string `json:"total_amount"`
		BuyerUserID  string `json:"buyer_user_id"`
		BuyerOpenID  string `json:"buyer_open_id"`
		BuyerLogonID string `json:"buyer_logon_id"`
	} `json:"alipay_trade_query_response"`
	Sign string `json:"sign"`
}

// payerID picks the payer identifier. buyer_user_id / buyer_id is the
// unmasked 2088 account number; merchants on the openid scheme get
// buyer_open_id instead. buyer_logon_id is masked ("159****5620") and is
// only a display fallback.
func payerID(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

type RefundInput struct {
	OrderNo     string
	RequestNo   string
	AmountCents int64
	Reason      string
}

// RefundResult reports whether Alipay moved the money. FundChanged=false
// with a nil error is not a failure: a repeated RequestNo answers "N"
// because the first call already refunded; confirm with QueryRefund.
type RefundResult struct {
	FundChanged bool
	RefundCents int64
}

// Refund calls alipay.trade.refund. Retrying with the same RequestNo never
// refunds twice.
func (c *Client) Refund(ctx context.Context, in RefundInput) (*RefundResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	bizContent := map[string]any{
		"out_trade_no":   in.OrderNo,
		"out_request_no": in.RequestNo,
		"refund_amount":  centsToYuan(in.AmountCents),
	}
	if in.Reason != "" {
		bizContent["refund_reason"] = in.Reason
	}
	var out refundResponse
	if err := c.request(ctx, "alipay.trade.refund", "", bizContent, &out); err != nil {
		return nil, err
	}
	resp := out.Response
	if resp.Code != "10000" {
		return nil, &APIError{Code: resp.Code, Msg: resp.Msg, SubCode: resp.SubCode, SubMsg: resp.SubMsg}
	}
	res := &RefundResult{FundChanged: resp.FundChange == "Y"}
	if strings.TrimSpace(resp.RefundFee) != "" {
		cents, err := yuanToCents(resp.RefundFee)
		if err != nil {
			return nil, err
		}
		res.RefundCents = cents
	}
	return res, nil
}

// QueryRefund calls alipay.trade.fastpay.refund.query and reports whether
// the refund RequestNo on OrderNo succeeded.
func (c *Client) QueryRefund(ctx context.Context, orderNo, requestNo string) (bool, error) {
	if c == nil {
		return false, ErrDisabled
	}
	bizContent := map[string]any{"out_trade_no": orderNo, "out_request_no": requestNo}
	var out refundQueryResponse
	if err := c.request(ctx, "alipay.trade.fastpay.refund.query", "", bizContent, &out); err != nil {
		return false, err
	}
	resp := out.Response
	if resp.Code != "10000" {
		return false, &APIError{Code: resp.Code, Msg: resp.Msg, SubCode: resp.SubCode, SubMsg: resp.SubMsg}
	}
	return resp.RefundStatus == "REFUND_SUCCESS", nil
}

// APIError is a business error answered by Alipay (code != 10000), as
// opposed to a transport failure where the outcome is unknown.
type APIError struct {
	Code, Msg, SubCode, SubMsg string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("alipay: code=%s msg=%s sub_code=%s sub_msg=%s", e.Code, e.Msg, e.SubCode, e.SubMsg)
}

type refundResponse struct {
	Response struct {
		Code       string `json:"code"`
		Msg        string `json:"msg"`
		SubCode    string `json:"sub_code"`
		SubMsg     string `json:"sub_msg"`
		FundChange string `json:"fund_change"`
		RefundFee  string `json:"refund_fee"`
	} `json:"alipay_trade_refund_response"`
}

type refundQueryResponse struct {
	Response struct {
		Code         string `json:"code"`
		Msg          string `json:"msg"`
		SubCode      string `json:"sub_code"`
		SubMsg       string `json:"sub_msg"`
		RefundStatus string `json:"refund_status"`
	} `json:"alipay_trade_fastpay_refund_query_response"`
}
