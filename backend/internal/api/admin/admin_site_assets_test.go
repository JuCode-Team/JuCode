package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func postSiteAsset(t *testing.T, h *Handler, filename string, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/site-assets", h.uploadSiteAsset)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(raw)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/site-assets", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	return res
}

func TestUploadSiteAssetStoresContentAddressedImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site-assets")
	h := &Handler{}
	h.SetSiteAssetDir(dir)
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	// The stored extension follows the sniffed content, not the client name.
	res := postSiteAsset(t, h, "qr.png", img.Bytes())
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	sum := sha256.Sum256(img.Bytes())
	name := hex.EncodeToString(sum[:])[:16] + ".jpg"
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &out)
	if out.URL != "/v1/public/site-assets/"+name {
		t.Fatalf("url = %q, want %q", out.URL, "/v1/public/site-assets/"+name)
	}
	stored, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(stored, img.Bytes()) {
		t.Fatalf("stored file mismatch: err=%v", err)
	}
}

func TestUploadSiteAssetRejectsNonImageAndOversize(t *testing.T) {
	h := &Handler{}
	h.SetSiteAssetDir(t.TempDir())
	if res := postSiteAsset(t, h, "x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)); res.Code != http.StatusBadRequest {
		t.Fatalf("svg status = %d, want 400", res.Code)
	}
	if res := postSiteAsset(t, h, "x.gif", []byte("GIF89a\x01\x00\x01\x00")); res.Code != http.StatusBadRequest {
		t.Fatalf("gif status = %d, want 400", res.Code)
	}
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, maxSiteAssetBytes)...)
	if res := postSiteAsset(t, h, "big.png", big); res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", res.Code)
	}
}
