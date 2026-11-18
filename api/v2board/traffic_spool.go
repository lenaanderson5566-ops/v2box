package panel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/google/uuid"
)

type trafficRecord struct {
	ID       int   `json:"userId"`
	Upload   int64 `json:"uploadBytes"`
	Download int64 `json:"downloadBytes"`
}
type trafficBatch struct {
	BatchID string          `json:"batchId"`
	Records []trafficRecord `json:"records"`
}

func (c *Client) spoolPath() string {
	root := c.trafficSpoolDirectory
	if root == "" {
		root = "/var/lib/v2box/traffic"
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", c.APIHost, c.NodeId)))
	return filepath.Join(root, fmt.Sprintf("%x", key))
}

// ReportUserTrafficReliable transfers ownership only after durable storage.
// Once retained, callers must never restore these bytes to live counters.
func (c *Client) ReportUserTrafficReliable(ctx context.Context, traffic []UserTraffic) (retained bool, err error) {
	if !c.v10 {
		if len(traffic) == 0 {
			return false, nil
		}
		return false, c.ReportUserTraffic(ctx, traffic)
	}
	c.trafficMu.Lock()
	defer c.trafficMu.Unlock()
	// Durable append is serialized, but network waits must not block new batches.
	outsideStorageLock := func(send func() error) error {
		c.trafficMu.Unlock()
		defer c.trafficMu.Lock()
		return send()
	}
	dir := c.spoolPath()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return false, err
	}
	if !c.trafficDedup.Load() {
		// Never replay durable batches against a downgraded/non-deduplicating panel.
		if len(files) > 0 {
			return false, fmt.Errorf("pending traffic requires panel batch deduplication support")
		}
		if len(traffic) == 0 {
			return false, nil
		}
		return false, outsideStorageLock(func() error { return c.ReportUserTraffic(ctx, traffic) })
	}
	if len(traffic) > 0 {
		var size int64
		for _, f := range files {
			info, e := os.Stat(f)
			if e != nil {
				return false, e
			}
			size += info.Size()
		}
		batch := trafficBatch{BatchID: uuid.NewString(), Records: make([]trafficRecord, 0, len(traffic))}
		for _, t := range traffic {
			batch.Records = append(batch.Records, trafficRecord{t.UID, t.Upload, t.Download})
		}
		raw, e := json.Marshal(batch)
		if e != nil {
			return false, e
		}
		if size+int64(len(raw)) > 64*1024*1024 {
			// Drain existing receipts even when no space remains for the current counters.
			if e := outsideStorageLock(func() error { return c.drainTraffic(ctx) }); e != nil {
				return false, e
			}
			return false, fmt.Errorf("traffic spool reached 64 MiB limit")
		}
		if e = os.MkdirAll(dir, 0700); e != nil {
			return false, e
		}
		f, e := os.CreateTemp(dir, ".pending-")
		if e != nil {
			return false, e
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		if e = f.Chmod(0600); e == nil {
			_, e = f.Write(raw)
		}
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return false, e
		}
		path := filepath.Join(dir, batch.BatchID+".json")
		if e = os.Rename(tmp, path); e != nil {
			return false, e
		}
		retained = true
		if runtime.GOOS != "windows" {
			for _, path := range []string{dir, filepath.Dir(dir)} {
				d, e := os.Open(path)
				if e != nil {
					return true, e
				}
				e = d.Sync()
				d.Close()
				if e != nil {
					return true, e
				}
			}
		}
	}
	return retained, outsideStorageLock(func() error { return c.drainTraffic(ctx) })
}

func (c *Client) drainTraffic(ctx context.Context) error {
	// One sender owns replay; concurrent reporters have already durably retained
	// their batches and leave delivery to this sender or a subsequent report tick.
	if !c.trafficDrainMu.TryLock() {
		return nil
	}
	defer c.trafficDrainMu.Unlock()
	c.trafficMu.Lock()
	files, err := filepath.Glob(filepath.Join(c.spoolPath(), "*.json"))
	c.trafficMu.Unlock()
	if err != nil {
		return err
	}
	sort.Strings(files)
	// Bound work per tick; subsequent ticks drain the remaining backlog, even with no new traffic.
	for i, path := range files {
		if i >= 16 {
			break
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var batch trafficBatch
		if e = json.Unmarshal(raw, &batch); e != nil {
			return fmt.Errorf("invalid pending traffic batch")
		}
		if _, e = uuid.Parse(batch.BatchID); e != nil || batch.Records == nil {
			return fmt.Errorf("invalid pending traffic batch")
		}
		if e = c.postV10Body(ctx, "traffic", batch); e != nil {
			return e
		}
		c.trafficMu.Lock()
		e = os.Remove(path)
		c.trafficMu.Unlock()
		if e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}
