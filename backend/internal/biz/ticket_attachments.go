package biz

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	maxTicketAttachmentBytes = 2 * 1024 * 1024
	maxTicketAttachments     = 3
)

var allowedTicketImageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Logs a JuCode Desktop bug report attaches: larger than a screenshot.
var allowedTicketLogTypes = map[string]string{
	"application/gzip": ".gz",
	"text/plain":       ".txt",
}

const maxTicketLogBytes = 8 * 1024 * 1024

var (
	ErrTicketAttachmentInvalid = errors.New("invalid ticket attachment")
	ErrTicketAttachmentGone    = errors.New("ticket attachment not found")
)

func (s *TicketSvc) SetAttachmentDir(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = "./data/ticket-attachments"
	}
	s.attachDir = dir
}

func (s *TicketSvc) persistAttachments(ticketID uuid.UUID, in []map[string]string) ([]map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxTicketAttachments {
		in = in[:maxTicketAttachments]
	}
	out := make([]map[string]string, 0, len(in))
	for _, raw := range in {
		item, err := s.persistOneAttachment(ticketID, raw)
		if err != nil {
			s.discardAttachments(ticketID, out)
			return nil, err
		}
		if item != nil {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *TicketSvc) persistOneAttachment(ticketID uuid.UUID, raw map[string]string) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	dataURL := strings.TrimSpace(raw["data_url"])
	if id := strings.TrimSpace(raw["id"]); id != "" && dataURL == "" {
		if !safeAttachmentID(id) {
			return nil, ErrTicketAttachmentInvalid
		}
		if _, err := os.Stat(filepath.Join(s.attachmentRoot(), ticketID.String(), id)); err != nil {
			return nil, ErrTicketAttachmentInvalid
		}
		name := strings.TrimSpace(raw["name"])
		typ := strings.TrimSpace(raw["type"])
		return map[string]string{"id": id, "name": name, "type": typ}, nil
	}
	if dataURL == "" {
		return nil, nil
	}
	typ, payload, err := parseDataURL(dataURL)
	if err != nil {
		return nil, err
	}
	ext, ok := allowedTicketImageTypes[typ]
	limit := maxTicketAttachmentBytes
	if !ok {
		ext, ok = allowedTicketLogTypes[typ]
		limit = maxTicketLogBytes
	}
	if !ok {
		return nil, ErrTicketAttachmentInvalid
	}
	if len(payload) == 0 || len(payload) > limit {
		return nil, ErrTicketAttachmentInvalid
	}
	id := uuid.NewString() + ext
	dir := filepath.Join(s.attachmentRoot(), ticketID.String())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(raw["name"])
	if name == "" {
		name = id
	}
	return map[string]string{"id": id, "name": name, "type": typ}, nil
}

func (s *TicketSvc) discardAttachments(ticketID uuid.UUID, atts []map[string]string) {
	for _, a := range atts {
		if a == nil {
			continue
		}
		id := strings.TrimSpace(a["id"])
		if !safeAttachmentID(id) {
			continue
		}
		_ = os.Remove(filepath.Join(s.attachmentRoot(), ticketID.String(), id))
	}
}

func (s *TicketSvc) OpenAttachment(ticketID uuid.UUID, fileID string) (string, string, error) {
	if !safeAttachmentID(fileID) {
		return "", "", ErrTicketAttachmentInvalid
	}
	path := filepath.Join(s.attachmentRoot(), ticketID.String(), fileID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", "", ErrTicketAttachmentGone
		}
		return "", "", err
	}
	typ := "application/octet-stream"
	ext := strings.ToLower(filepath.Ext(fileID))
	for _, types := range []map[string]string{allowedTicketImageTypes, allowedTicketLogTypes} {
		for mime, e := range types {
			if e == ext {
				typ = mime
			}
		}
	}
	return path, typ, nil
}

func AttachmentViews(ticketID uuid.UUID, atts []map[string]string, staff bool) []map[string]string {
	if len(atts) == 0 {
		return atts
	}
	prefix := "/v1/me/tickets/" + ticketID.String() + "/attachments/"
	if staff {
		prefix = "/v1/cs/tickets/" + ticketID.String() + "/attachments/"
	}
	out := make([]map[string]string, 0, len(atts))
	for _, a := range atts {
		if a == nil {
			continue
		}
		item := map[string]string{
			"name": strings.TrimSpace(a["name"]),
			"type": strings.TrimSpace(a["type"]),
		}
		if id := strings.TrimSpace(a["id"]); id != "" {
			item["id"] = id
			item["url"] = prefix + id
		} else if dataURL := strings.TrimSpace(a["data_url"]); dataURL != "" {
			// Legacy rows still in the JSON column as a data URL.
			item["data_url"] = dataURL
		}
		out = append(out, item)
	}
	return out
}

func (s *TicketSvc) attachmentRoot() string {
	if s != nil && strings.TrimSpace(s.attachDir) != "" {
		return s.attachDir
	}
	return "./data/ticket-attachments"
}

func parseDataURL(v string) (string, []byte, error) {
	if !strings.HasPrefix(v, "data:") {
		return "", nil, ErrTicketAttachmentInvalid
	}
	comma := strings.IndexByte(v, ',')
	if comma < 0 {
		return "", nil, ErrTicketAttachmentInvalid
	}
	meta := v[5:comma]
	payload := v[comma+1:]
	typ, _, _ := strings.Cut(meta, ";")
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !strings.Contains(strings.ToLower(meta), "base64") {
		return "", nil, ErrTicketAttachmentInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return "", nil, ErrTicketAttachmentInvalid
		}
	}
	return typ, raw, nil
}

func safeAttachmentID(id string) bool {
	if id == "" || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
