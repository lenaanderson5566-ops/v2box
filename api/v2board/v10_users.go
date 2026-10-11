package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (c *Client) getV10Users(ctx context.Context) ([]UserInfo, error) {
	r, err := c.client.R().SetContext(ctx).SetHeader("If-None-Match", c.userEtag).Get(c.nodePath("users"))
	if err != nil {
		return nil, err
	}
	if r.StatusCode() == 304 {
		return nil, nil
	}
	if r.StatusCode() != 200 {
		return nil, fmt.Errorf("V10 users HTTP %d", r.StatusCode())
	}
	var body struct {
		Data *[]struct {
			ID      int    `json:"id"`
			UUID    string `json:"uuid"`
			Speed   int    `json:"speedLimit"`
			Devices int    `json:"deviceLimit"`
		} `json:"data"`
	}
	if err = json.Unmarshal(r.Body(), &body); err != nil {
		return nil, err
	}
	if body.Data == nil {
		return nil, fmt.Errorf("V10 users missing data array")
	}
	users := make([]UserInfo, 0, len(*body.Data))
	ids, uuids := map[int]bool{}, map[string]bool{}
	for _, u := range *body.Data {
		if u.ID <= 0 || u.UUID == "" || u.Speed < 0 || u.Devices < 0 || ids[u.ID] || uuids[u.UUID] {
			return nil, fmt.Errorf("invalid V10 user")
		}
		ids[u.ID], uuids[u.UUID] = true, true
		users = append(users, UserInfo{Id: u.ID, Uuid: u.UUID, SpeedLimit: u.Speed, DeviceLimit: u.Devices})
	}
	c.userEtag = r.Header().Get("ETag")
	return users, nil
}

func (c *Client) getV10Alive(ctx context.Context) (map[int]int, error) {
	r, err := c.client.R().SetContext(ctx).Get(c.nodePath("online-counts"))
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != 200 {
		return nil, fmt.Errorf("V10 online counts HTTP %d", r.StatusCode())
	}
	var body struct {
		Data *[]struct {
			ID    int `json:"userId"`
			Count int `json:"count"`
		} `json:"data"`
	}
	if err = json.Unmarshal(r.Body(), &body); err != nil {
		return nil, err
	}
	if body.Data == nil {
		return nil, fmt.Errorf("V10 online counts missing data")
	}
	out := map[int]int{}
	for _, v := range *body.Data {
		if v.ID <= 0 || v.Count < 0 {
			return nil, fmt.Errorf("invalid online count")
		}
		out[v.ID] = v.Count
	}
	return out, nil
}

func (c *Client) postV10(ctx context.Context, resource string, records any) error {
	r, err := c.client.R().SetContext(ctx).SetBody(map[string]any{"records": records}).Post(c.nodePath(resource))
	if err != nil {
		return err
	}
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return fmt.Errorf("V10 %s HTTP %d", resource, r.StatusCode())
	}
	var body struct {
		Data struct {
			Accepted bool `json:"accepted"`
		} `json:"data"`
	}
	if err = json.Unmarshal(r.Body(), &body); err != nil {
		return err
	}
	if !body.Data.Accepted {
		return fmt.Errorf("V10 %s not acknowledged", resource)
	}
	return nil
}

func (c *Client) reportV10Traffic(ctx context.Context, traffic []UserTraffic) error {
	type record struct {
		ID       int   `json:"userId"`
		Upload   int64 `json:"uploadBytes"`
		Download int64 `json:"downloadBytes"`
	}
	records := make([]record, 0, len(traffic))
	for _, t := range traffic {
		records = append(records, record{t.UID, t.Upload, t.Download})
	}
	return c.postV10(ctx, "traffic", records)
}

func (c *Client) reportV10Online(ctx context.Context, data *map[int][]string) error {
	type record struct {
		ID  int      `json:"userId"`
		IPs []string `json:"ips"`
	}
	records := []record{}
	if data != nil {
		for id, ips := range *data {
			clean := make([]string, 0, len(ips))
			for _, ip := range ips {
				clean = append(clean, strings.SplitN(ip, "_", 2)[0])
			}
			records = append(records, record{id, clean})
		}
	}
	return c.postV10(ctx, "online-users", records)
}
