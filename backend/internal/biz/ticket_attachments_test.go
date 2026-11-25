package biz

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPersistAndOpenAttachment(t *testing.T) {
	dir := t.TempDir()
	s := &TicketSvc{}
	s.SetAttachmentDir(dir)
	ticketID := uuid.New()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	out, err := s.persistAttachments(ticketID, []map[string]string{{
		"name":     "shot.png",
		"type":     "image/png",
		"data_url": dataURL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["id"] == "" || strings.Contains(out[0]["data_url"], "base64") {
		t.Fatalf("expected disk id without data_url, got %#v", out)
	}
	path, typ, err := s.OpenAttachment(ticketID, out[0]["id"])
	if err != nil {
		t.Fatal(err)
	}
	if typ != "image/png" {
		t.Fatalf("type %s", typ)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(png) {
		t.Fatal("payload mismatch")
	}
	views := AttachmentViews(ticketID, out, false)
	if !strings.Contains(views[0]["url"], "/v1/me/tickets/"+ticketID.String()+"/attachments/") {
		t.Fatalf("url %s", views[0]["url"])
	}
}

func TestOpenAttachmentRejectsTraversal(t *testing.T) {
	s := &TicketSvc{}
	s.SetAttachmentDir(t.TempDir())
	if _, _, err := s.OpenAttachment(uuid.New(), "../secret"); err != ErrTicketAttachmentInvalid {
		t.Fatalf("got %v", err)
	}
}

func TestPersistRejectsOtherTypes(t *testing.T) {
	s := &TicketSvc{}
	s.SetAttachmentDir(t.TempDir())
	_, err := s.persistAttachments(uuid.New(), []map[string]string{{
		"name":     "x.pdf",
		"data_url": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("hi")),
	}})
	if err != ErrTicketAttachmentInvalid {
		t.Fatalf("got %v", err)
	}
}

// A desktop bug report attaches its logs, gzipped, up to 8 MB; a
// screenshot stays at 2 MB.
func TestPersistTakesLogsLargerThanImages(t *testing.T) {
	s := &TicketSvc{}
	s.SetAttachmentDir(t.TempDir())
	ticketID := uuid.New()
	logs := make([]byte, 3<<20)
	out, err := s.persistAttachments(ticketID, []map[string]string{{
		"name":     "jucode-logs.txt.gz",
		"data_url": "data:application/gzip;base64," + base64.StdEncoding.EncodeToString(logs),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, typ, err := s.OpenAttachment(ticketID, out[0]["id"]); err != nil || typ != "application/gzip" {
		t.Fatalf("open: %s %v", typ, err)
	}
	_, err = s.persistAttachments(ticketID, []map[string]string{{
		"name":     "big.png",
		"data_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(logs),
	}})
	if err != ErrTicketAttachmentInvalid {
		t.Fatalf("a 3 MB image: got %v", err)
	}
	_, err = s.persistAttachments(ticketID, []map[string]string{{
		"name":     "huge.gz",
		"data_url": "data:application/gzip;base64," + base64.StdEncoding.EncodeToString(make([]byte, 9<<20)),
	}})
	if err != ErrTicketAttachmentInvalid {
		t.Fatalf("a 9 MB log: got %v", err)
	}
}

func TestPersistRejectsUnknownAttachmentID(t *testing.T) {
	s := &TicketSvc{}
	s.SetAttachmentDir(t.TempDir())
	_, err := s.persistAttachments(uuid.New(), []map[string]string{{
		"id":   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.png",
		"name": "stolen.png",
		"type": "image/png",
	}})
	if err != ErrTicketAttachmentInvalid {
		t.Fatalf("got %v", err)
	}
}

func TestPersistKeepsExistingAttachmentID(t *testing.T) {
	dir := t.TempDir()
	s := &TicketSvc{}
	s.SetAttachmentDir(dir)
	ticketID := uuid.New()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	out, err := s.persistAttachments(ticketID, []map[string]string{{
		"name":     "shot.png",
		"type":     "image/png",
		"data_url": dataURL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.persistAttachments(ticketID, []map[string]string{{
		"id":   out[0]["id"],
		"name": "shot.png",
		"type": "image/png",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if again[0]["id"] != out[0]["id"] {
		t.Fatalf("id changed: %#v", again)
	}
}

func TestPersistCleansPartialOnError(t *testing.T) {
	dir := t.TempDir()
	s := &TicketSvc{}
	s.SetAttachmentDir(dir)
	ticketID := uuid.New()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	okURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	_, err := s.persistAttachments(ticketID, []map[string]string{
		{"name": "ok.png", "type": "image/png", "data_url": okURL},
		{"name": "bad.pdf", "data_url": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("x"))},
	})
	if err != ErrTicketAttachmentInvalid {
		t.Fatalf("got %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ticketID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("orphans left: %v", entries)
	}
}
