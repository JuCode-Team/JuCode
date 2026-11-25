package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/region"
)

// FileOutbox is intentionally small and durable: each event is written to a
// temporary file, fsynced, and atomically renamed into the pending directory.
// Redis remains the hot-path ledger; this queue is what prevents a Redis or
// control-channel outage from losing accounting events.
type FileOutbox struct {
	dir string
	mu  sync.Mutex
}

func NewFileOutbox(dir string) (*FileOutbox, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("edge outbox directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create edge outbox: %w", err)
	}
	return &FileOutbox{dir: dir}, nil
}

func (o *FileOutbox) Enqueue(ctx context.Context, event region.UsageEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.EventID == uuid.Nil {
		event.EventID = uuidNew()
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	tmp, err := os.CreateTemp(o.dir, ".pending-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	final := filepath.Join(o.dir, event.EventID.String()+".json")
	if err := os.Rename(name, final); err != nil {
		return err
	}
	return syncDir(o.dir)
}

func (o *FileOutbox) Pending(ctx context.Context, limit int) ([]region.UsageEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}
	files := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	out := make([]region.UsageEvent, 0, min(limit, len(files)))
	for _, entry := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if len(out) >= limit {
			break
		}
		data, err := os.ReadFile(filepath.Join(o.dir, entry.Name()))
		if err != nil {
			return out, err
		}
		var event region.UsageEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return out, fmt.Errorf("decode outbox %s: %w", entry.Name(), err)
		}
		out = append(out, event)
	}
	return out, nil
}

func (o *FileOutbox) Ack(ctx context.Context, events []region.UsageEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, event := range events {
		if event.EventID == uuid.Nil {
			continue
		}
		path := filepath.Join(o.dir, event.EventID.String()+".json")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDir(o.dir)
}

func uuidNew() uuid.UUID { return uuid.New() }

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
