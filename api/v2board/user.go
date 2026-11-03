package panel

import (
	"context"
	"fmt"
	"io"
	"strings"

	"encoding/json/v2"

	"github.com/vmihailenco/msgpack/v5"
)

type OnlineUser struct {
	UID int
	IP  string
}

type UserInfo struct {
	Id          int    `json:"id" msgpack:"id"`
	Uuid        string `json:"uuid" msgpack:"uuid"`
	SpeedLimit  int    `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit int    `json:"device_limit" msgpack:"device_limit"`
}

type UserListBody struct {
	Users []UserInfo `json:"users" msgpack:"users"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

// GetUserList will pull user from v2board
func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	if c.v10 {
		return c.getV10Users(ctx)
	}
	const path = "/api/v1/server/UniProxy/user"
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer r.RawResponse.Body.Close()

	if r.StatusCode() == 304 {
		return nil, nil
	}
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("user API returned HTTP %d", r.StatusCode())
	}
	userlist := &UserListBody{}
	limit := c.client.ResponseBodyLimit
	if limit <= 0 {
		limit = maxPanelResponseBytes
	}
	body := &io.LimitedReader{R: r.RawResponse.Body, N: int64(limit) + 1}
	if strings.Contains(r.Header().Get("Content-Type"), "application/x-msgpack") {
		decoder := msgpack.NewDecoder(body)
		if err := decodeLegacyUserMessagePack(decoder, userlist); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	} else {
		if err := json.UnmarshalRead(body, userlist); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	}
	if body.N <= 0 {
		return nil, fmt.Errorf("user list exceeds response limit")
	}
	if userlist.Users == nil {
		return nil, fmt.Errorf("user list missing users array")
	}
	ids, uuids := map[int]bool{}, map[string]bool{}
	for _, u := range userlist.Users {
		if u.Id <= 0 || u.Uuid == "" || u.SpeedLimit < 0 || u.DeviceLimit < 0 || ids[u.Id] || uuids[u.Uuid] {
			return nil, fmt.Errorf("invalid or duplicate panel user %d", u.Id)
		}
		ids[u.Id] = true
		uuids[u.Uuid] = true
	}
	c.userEtag = r.Header().Get("ETag")
	return userlist.Users, nil
}

// Decode users incrementally: an untrusted MessagePack array length must not
// cause the decoder to preallocate the complete authentication directory.
func decodeLegacyUserMessagePack(decoder *msgpack.Decoder, out *UserListBody) error {
	fields, err := decoder.DecodeMapLen()
	if err != nil {
		return err
	}
	if fields < 0 {
		return fmt.Errorf("missing user list envelope")
	}
	for i := 0; i < fields; i++ {
		key, err := decoder.DecodeString()
		if err != nil {
			return err
		}
		if key != "users" {
			if err := decoder.Skip(); err != nil {
				return err
			}
			continue
		}
		if out.Users != nil {
			return fmt.Errorf("duplicate users field")
		}
		count, err := decoder.DecodeArrayLen()
		if err != nil {
			return err
		}
		if count < 0 {
			return fmt.Errorf("missing users array")
		}
		out.Users = make([]UserInfo, 0)
		for j := 0; j < count; j++ {
			var user UserInfo
			if err := decoder.Decode(&user); err != nil {
				return err
			}
			out.Users = append(out.Users, user)
		}
	}
	if _, err := decoder.PeekCode(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("read user list ending: %w", err)
		}
		return fmt.Errorf("unexpected data after user list")
	}
	return nil
}

// GetUserAlive will fetch the alive_ip count for users
func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	if c.v10 {
		return c.getV10Alive(ctx)
	}
	const path = "/api/v1/server/UniProxy/alivelist"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil alive response")
	}
	// Older legacy panels may not implement this optional endpoint.
	if r.StatusCode() == 404 {
		c.AliveMap = &AliveMap{Alive: map[int]int{}}
		return c.AliveMap.Alive, nil
	}
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("alive API returned HTTP %d", r.StatusCode())
	}
	defer r.RawResponse.Body.Close()
	alive := &AliveMap{}
	if err := json.Unmarshal(r.Body(), alive); err != nil {
		return nil, fmt.Errorf("decode alive list: %w", err)
	}
	if alive.Alive == nil {
		return nil, fmt.Errorf("alive list missing alive map")
	}
	for id, count := range alive.Alive {
		if id <= 0 || count < 0 {
			return nil, fmt.Errorf("invalid online count")
		}
	}
	c.AliveMap = alive
	return alive.Alive, nil
}

type UserTraffic struct {
	UID      int
	Upload   int64
	Download int64
}

// ReportUserTraffic reports the user traffic
func (c *Client) ReportUserTraffic(ctx context.Context, userTraffic []UserTraffic) error {
	if c.v10 {
		return c.reportV10Traffic(ctx, userTraffic)
	}
	data := make(map[int][]int64, len(userTraffic))
	for i := range userTraffic {
		data[userTraffic[i].UID] = []int64{userTraffic[i].Upload, userTraffic[i].Download}
	}
	const path = "/api/v1/server/UniProxy/push"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	if r == nil || r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return fmt.Errorf("panel report rejected")
	}
	return nil
}

func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data *map[int][]string) error {
	if c.v10 {
		return c.reportV10Online(ctx, data)
	}
	const path = "/api/v1/server/UniProxy/alive"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)

	if err != nil {
		return err
	}

	if r == nil || r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return fmt.Errorf("panel report rejected")
	}
	return nil
}

func (c *Client) InvalidateUserCache() { c.userEtag = "" }
