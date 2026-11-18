package limiter

import (
	"errors"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"github.com/lenaanderson5566-ops/v2box/common/rate"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var limitLock sync.RWMutex
var limiters = map[string]*Limiter{}

func Init() { limitLock.Lock(); defer limitLock.Unlock(); limiters = map[string]*Limiter{} }

type UserLimitInfo struct {
	UID, SpeedLimit, DeviceLimit, DynamicSpeedLimit int
	ExpireTime                                      int64
}
type Limiter struct {
	shards [32]limitShard
	alive  atomic.Pointer[map[int]int]
}

// Each user owns one shard, including its aggregate speed bucket and IP state.
type limitShard struct {
	mu       sync.Mutex
	users    map[string]UserLimitInfo
	buckets  map[string]*rate.DynamicBucket
	rates    map[string]int64
	online   map[string]map[string]time.Time
	observed map[string]map[string]bool
}

func AddLimiter(_ string, tag string, users []panel.UserInfo, alive map[int]int) *Limiter {
	l := &Limiter{}
	for i := range l.shards {
		l.shards[i] = limitShard{users: map[string]UserLimitInfo{}, buckets: map[string]*rate.DynamicBucket{}, rates: map[string]int64{}, online: map[string]map[string]time.Time{}, observed: map[string]map[string]bool{}}
	}
	l.UpdateUser(tag, users, nil, nil)
	l.SetAlive(alive)
	limitLock.Lock()
	limiters[tag] = l
	limitLock.Unlock()
	return l
}
func GetLimiter(tag string) (*Limiter, error) {
	limitLock.RLock()
	defer limitLock.RUnlock()
	l, ok := limiters[tag]
	if !ok {
		return nil, errors.New("limiter not found")
	}
	return l, nil
}
func DeleteLimiter(tag string) { limitLock.Lock(); delete(limiters, tag); limitLock.Unlock() }
func (l *Limiter) shard(user string) *limitShard {
	var hash uint32 = 2166136261
	for i := 0; i < len(user); i++ {
		hash = (hash ^ uint32(user[i])) * 16777619
	}
	return &l.shards[hash%uint32(len(l.shards))]
}
func (l *Limiter) SetAlive(alive map[int]int) {
	snapshot := make(map[int]int, len(alive))
	for id, n := range alive {
		snapshot[id] = n
	}
	l.alive.Store(&snapshot)
}
func (l *Limiter) UpdateUser(tag string, added, deleted, modified []panel.UserInfo) {
	for _, u := range deleted {
		k := format.UserTag(tag, u.Uuid)
		shard := l.shard(k)
		shard.mu.Lock()
		delete(shard.users, k)
		delete(shard.buckets, k)
		delete(shard.rates, k)
		delete(shard.online, k)
		delete(shard.observed, k)
		shard.mu.Unlock()
	}
	for _, group := range [][]panel.UserInfo{added, modified} {
		for _, u := range group {
			k := format.UserTag(tag, u.Uuid)
			shard := l.shard(k)
			shard.mu.Lock()
			shard.users[k] = UserLimitInfo{UID: u.Id, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit}
			shard.mu.Unlock()
		}
	}
}
func (lim *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expires time.Time) error {
	k := format.UserTag(tag, uuid)
	l := lim.shard(k)
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.users[k]
	if !ok {
		return errors.New("user not found")
	}
	u.DynamicSpeedLimit = limit
	u.ExpireTime = expires.Unix()
	l.users[k] = u
	return nil
}
func (lim *Limiter) CheckLimit(user, ip string, _ bool) (*rate.DynamicBucket, bool) {
	alive := lim.alive.Load()
	l := lim.shard(user)
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.users[user]
	if !ok {
		return nil, true
	}
	ip = strings.TrimPrefix(ip, "::ffff:")
	now := time.Now()
	if l.online[user] == nil {
		l.online[user] = map[string]time.Time{}
	}
	ips := l.online[user]
	// Established devices are the packet hot path. Scan for expiry only when
	// admitting a new/expired device; reports also collect inactive user state.
	if last, known := ips[ip]; !known || now.Sub(last) > 2*time.Minute {
		pruneIPs(ips, now)
		remote := 0
		if alive != nil {
			remote = (*alive)[u.UID]
		}
		if u.DeviceLimit > 0 && max(len(ips), remote) >= u.DeviceLimit {
			return nil, true
		}
	}
	ips[ip] = now
	if l.observed[user] == nil {
		l.observed[user] = map[string]bool{}
	}
	l.observed[user][ip] = true
	dynamic := u.DynamicSpeedLimit
	if u.ExpireTime != 0 && now.Unix() > u.ExpireTime {
		dynamic = 0
	}
	speed := int64(determineSpeedLimit(u.SpeedLimit, dynamic)) * 1000000 / 8
	if speed <= 0 {
		return nil, false
	}
	b := l.buckets[user]
	if b == nil {
		b = rate.NewDynamicBucket(speed)
		l.buckets[user] = b
	} else if l.rates[user] != speed {
		b.Update(speed)
	}
	l.rates[user] = speed
	return b, false
}
func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	out := []panel.OnlineUser{}
	now := time.Now()
	for i := range l.shards {
		shard := &l.shards[i]
		shard.mu.Lock()
		for user, ips := range shard.observed {
			if u, ok := shard.users[user]; ok {
				for ip := range ips {
					out = append(out, panel.OnlineUser{UID: u.UID, IP: ip})
				}
			}
		}
		shard.observed = map[string]map[string]bool{}
		for user, ips := range shard.online {
			pruneIPs(ips, now)
			if len(ips) == 0 {
				delete(shard.online, user)
			}
		}
		shard.mu.Unlock()
	}
	return &out, nil
}

func pruneIPs(ips map[string]time.Time, now time.Time) {
	for ip, last := range ips {
		if now.Sub(last) > 2*time.Minute {
			delete(ips, ip)
		}
	}
}
