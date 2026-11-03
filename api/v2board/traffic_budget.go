package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type TrafficBudget struct {
	ID            int    `json:"id"`
	LimitBytes    int64  `json:"limitBytes"`
	UsedBytes     int64  `json:"usedBytes"`
	CountingMode  string `json:"countingMode"`
	PeriodStart   string `json:"periodStart"`
	PeriodEndUnix int64  `json:"periodEndUnix"`
}

func (c *Client) GetTrafficBudget(ctx context.Context) (*TrafficBudget, error) {
	if !c.budgetSupported.Load() {
		return nil, nil
	}
	r, err := c.client.R().SetContext(ctx).Get(c.nodePath("traffic-budget"))
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != 200 {
		return nil, fmt.Errorf("traffic budget HTTP %d", r.StatusCode())
	}
	var body struct {
		Data *TrafficBudget `json:"data"`
	}
	if err = json.Unmarshal(r.Body(), &body); err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(r.Body(), &envelope); err != nil {
		return nil, err
	}
	if _, ok := envelope["data"]; !ok {
		return nil, fmt.Errorf("traffic budget missing data")
	}
	b := body.Data
	if b != nil {
		if b.ID <= 0 || b.LimitBytes < 0 || b.UsedBytes < 0 || b.PeriodStart == "" || b.PeriodEndUnix <= 0 || (b.CountingMode != "both" && b.CountingMode != "upload" && b.CountingMode != "download") {
			return nil, fmt.Errorf("invalid traffic budget")
		}
		raw, e := os.ReadFile(filepath.Join(c.spoolPath(), "quota-state"))
		if e == nil {
			var old TrafficBudget
			if json.Unmarshal(raw, &old) == nil && old.ID == b.ID && old.PeriodStart == b.PeriodStart && old.CountingMode == b.CountingMode && old.UsedBytes > b.UsedBytes {
				b.UsedBytes = old.UsedBytes
			}
		}
	}
	return b, nil
}
func (c *Client) SaveTrafficBudget(b *TrafficBudget) error {
	if b == nil {
		return nil
	}
	dir := c.spoolPath()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".quota-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "quota-state"))
}
