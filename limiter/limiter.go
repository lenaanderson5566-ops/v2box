package limiter

import (
	"errors"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/format"
	"github.com/lenaanderson5566-ops/v2box/common/rate"
	"strings"
	"sync"
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
	mu       sync.Mutex
	users    map[string]UserLimitInfo
	buckets  map[string]*rate.DynamicBucket
	rates    map[string]int64
	alive    map[int]int
	online   map[string]map[string]time.Time
	observed map[string]map[string]bool
}

func AddLimiter(_ string, tag string, users []panel.UserInfo, alive map[int]int) *Limiter {
	l := &Limiter{users: map[string]UserLimitInfo{}, buckets: map[string]*rate.DynamicBucket{}, rates: map[string]int64{}, alive: map[int]int{}, online: map[string]map[string]time.Time{}, observed: map[string]map[string]bool{}}
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
func (l *Limiter) SetAlive(alive map[int]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.alive = make(map[int]int, len(alive))
	for id, n := range alive {
		l.alive[id] = n
	}
}
func (l *Limiter) UpdateUser(tag string, added, deleted, modified []panel.UserInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, u := range deleted {
		k := format.UserTag(tag, u.Uuid)
		delete(l.users, k)
		delete(l.buckets, k)
		delete(l.rates, k)
		delete(l.online, k)
		delete(l.observed, k)
	}
	for _, group := range [][]panel.UserInfo{added, modified} {
		for _, u := range group {
			k := format.UserTag(tag, u.Uuid)
			l.users[k] = UserLimitInfo{UID: u.Id, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit}
		}
	}
}
func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expires time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := format.UserTag(tag, uuid)
	u, ok := l.users[k]
	if !ok {
		return errors.New("user not found")
	}
	u.DynamicSpeedLimit = limit
	u.ExpireTime = expires.Unix()
	l.users[k] = u
	return nil
}
func (l *Limiter) CheckLimit(user, ip string, _ bool) (*rate.DynamicBucket, bool) {
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
		if u.DeviceLimit > 0 && max(len(ips), l.alive[u.UID]) >= u.DeviceLimit {
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
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []panel.OnlineUser{}
	for user, ips := range l.observed {
		u, ok := l.users[user]
		if ok {
			for ip := range ips {
				out = append(out, panel.OnlineUser{UID: u.UID, IP: ip})
			}
		}
	}
	l.observed = map[string]map[string]bool{}
	now := time.Now()
	for user, ips := range l.online {
		pruneIPs(ips, now)
		if len(ips) == 0 {
			delete(l.online, user)
		}
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
