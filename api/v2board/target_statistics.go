package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
)

type TargetPolicy struct {
	Enabled      bool   `json:"enabled"`
	Version      string `json:"version"`
	LeaseSeconds int    `json:"leaseSeconds"`
}
type TargetRecord struct {
	UserID        int    `json:"userId"`
	Target        string `json:"target"`
	Type          string `json:"type"`
	Connections   int64  `json:"connections"`
	UploadBytes   int64  `json:"uploadBytes"`
	DownloadBytes int64  `json:"downloadBytes"`
}
type TargetBatch struct {
	Version string         `json:"version"`
	BatchID string         `json:"batchId"`
	Records []TargetRecord `json:"records"`
	Dropped int64          `json:"dropped"`
}

func (c *Client) GetTargetPolicy(ctx context.Context) (TargetPolicy, error) {
	var body struct {
		Data *TargetPolicy `json:"data"`
	}
	if !c.v10 {
		return TargetPolicy{}, nil
	}
	r, err := c.client.R().SetContext(ctx).SetDoNotParseResponse(true).Get(c.nodePath("target-statistics/policy"))
	if err != nil {
		return TargetPolicy{}, err
	}
	defer r.RawBody().Close()
	if r.StatusCode() == 404 {
		return TargetPolicy{}, nil
	} // Older panels do not collect.
	if r.StatusCode() != 200 {
		return TargetPolicy{}, fmt.Errorf("target policy HTTP %d", r.StatusCode())
	}
	data, err := io.ReadAll(io.LimitReader(r.RawBody(), 4097))
	if err != nil || len(data) > 4096 {
		return TargetPolicy{}, fmt.Errorf("oversized or unreadable target response")
	}
	if err = json.Unmarshal(data, &body); err != nil || body.Data == nil {
		return TargetPolicy{}, fmt.Errorf("invalid target policy")
	}
	p := *body.Data
	_, versionErr := uuid.Parse(p.Version)
	if p.Enabled && (versionErr != nil || len(p.Version) != 36 || p.LeaseSeconds < 1 || p.LeaseSeconds > 120) {
		return TargetPolicy{}, fmt.Errorf("invalid target lease")
	}
	return p, nil
}
func (c *Client) ReportTargets(ctx context.Context, batch *TargetBatch) (bool, error) {
	if !c.v10 {
		return false, nil
	}
	r, err := c.client.R().SetContext(ctx).SetDoNotParseResponse(true).SetBody(batch).Post(c.nodePath("target-statistics"))
	if err != nil {
		return false, err
	}
	defer r.RawBody().Close()
	if r.StatusCode() != 200 {
		return false, fmt.Errorf("target report HTTP %d", r.StatusCode())
	}
	var body struct {
		Data *struct {
			Accepted *bool `json:"accepted"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(r.RawBody(), 4097))
	if err != nil || len(data) > 4096 {
		return false, fmt.Errorf("oversized or unreadable target response")
	}
	if err = json.Unmarshal(data, &body); err != nil || body.Data == nil || body.Data.Accepted == nil {
		return false, fmt.Errorf("invalid target acknowledgement")
	}
	return *body.Data.Accepted, nil
}
