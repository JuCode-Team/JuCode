package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gin-gonic/gin"
)

// siteAssetName matches exactly what the admin upload produces (sha256
// prefix + extension), so no other file under the directory, and no path
// traversal, can be requested.
const defaultSiteAssetDir = "./data/ticket-attachments/site-assets"

var siteAssetName = regexp.MustCompile(`^[0-9a-f]{16}\.(png|jpg|webp)$`)

func mountSiteAssets(rg *gin.RouterGroup, dir string) {
	rg.GET("/public/site-assets/:name", func(c *gin.Context) {
		name := c.Param("name")
		if !siteAssetName.MatchString(name) {
			c.JSON(http.StatusNotFound, gin.H{"error": "asset not found"})
			return
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "asset not found"})
			return
		}
		c.Header("Cache-Control", "public, max-age=86400")
		c.File(path)
	})
}
