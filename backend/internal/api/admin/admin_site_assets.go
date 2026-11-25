package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

const maxSiteAssetBytes = 2 << 20

// siteAssetExts maps the sniffed content type to the stored extension.
// Only raster formats a browser renders safely in <img>; SVG is excluded
// because it can carry script when opened directly.
var siteAssetExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

// SetSiteAssetDir sets where POST /site-assets stores images; the public
// GET /v1/public/site-assets/:name serves from the same directory.
func (h *Handler) SetSiteAssetDir(dir string) { h.siteAssetDir = dir }

// uploadSiteAsset stores a small site image (e.g. a group QR code)
// under a content-addressed name so re-uploads of the same file dedupe.
func (h *Handler) uploadSiteAsset(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	if fileHeader.Size > maxSiteAssetBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "image must be at most 2MB"})
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to open file"})
		return
	}
	defer src.Close()
	raw, err := io.ReadAll(io.LimitReader(src, maxSiteAssetBytes+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return
	}
	if len(raw) > maxSiteAssetBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "image must be at most 2MB"})
		return
	}
	ext, ok := siteAssetExts[http.DetectContentType(raw)]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image must be png, jpg or webp"})
		return
	}
	sum := sha256.Sum256(raw)
	name := hex.EncodeToString(sum[:])[:16] + ext
	if err := os.MkdirAll(h.siteAssetDir, 0o750); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare asset storage"})
		return
	}
	if err := os.WriteFile(filepath.Join(h.siteAssetDir, name), raw, 0o640); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store asset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": "/v1/public/site-assets/" + name})
}
