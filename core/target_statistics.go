package core

import (
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	M "github.com/sagernet/sing/common/metadata"
	"golang.org/x/net/idna"
)

const targetShardCount = 32
const targetRowLimit = 500
const targetCacheEntryLimit = 8192
const targetCacheMeterLimit = 1024
const targetPendingTTL = 5 * time.Minute

type targetKey struct {
	user         int
	kind, target string
}
type targetShard struct {
	mu     sync.Mutex
	rows   map[targetKey]*panel.TargetRecord
	meters map[*meter]struct{}
}
type targetState struct {
	version      string
	generation   uint64
	until        atomic.Int64
	enabled      atomic.Bool
	count        atomic.Int64
	cacheEntries atomic.Int64
	cacheMeters  atomic.Int64
	dropped      atomic.Int64
	shards       [targetShardCount]targetShard
	// Pending batches are immutable and only managed under targetTracker.mu.
	pending   *panel.TargetBatch
	pendingAt time.Time
}
type targetView struct{ nodes map[string]*targetState }
type targetTracker struct {
	active     atomic.Bool
	view       atomic.Pointer[targetView]
	mu         sync.Mutex
	nodes      map[string]*targetState
	generation uint64
}

func (s *targetState) lock() {
	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
}
func (s *targetState) unlock() {
	for i := len(s.shards) - 1; i >= 0; i-- {
		s.shards[i].mu.Unlock()
	}
}
func (s *targetState) drop(n int64) {
	for {
		old := s.dropped.Load()
		if old >= 1000000000 || s.dropped.CompareAndSwap(old, min(int64(1000000000), old+n)) {
			return
		}
	}
}
func (s *targetState) retire() {
	// Drain old recorders before publishing a replacement generation. This also
	// protects each meter cache across policy changes without a per-packet lock.
	s.lock()
	s.enabled.Store(false)
	for i := range s.shards {
		s.shards[i].rows = nil
		for m := range s.shards[i].meters {
			m.targetNames = nil
			m.targetSeen = nil
		}
		s.shards[i].meters = nil
	}
	s.pending = nil
	s.cacheEntries.Store(0)
	s.cacheMeters.Store(0)
	s.unlock()
}
func (t *targetTracker) publish() {
	nodes := make(map[string]*targetState, len(t.nodes))
	for tag, s := range t.nodes {
		nodes[tag] = s
	}
	t.view.Store(&targetView{nodes: nodes})
	t.active.Store(len(nodes) > 0)
}
func (t *targetTracker) policy(tag string, p panel.TargetPolicy) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nodes == nil {
		t.nodes = map[string]*targetState{}
	}
	old := t.nodes[tag]
	now := time.Now()
	if !p.Enabled || p.Version == "" || p.LeaseSeconds < 1 || p.LeaseSeconds > 120 {
		if old != nil {
			old.retire()
			delete(t.nodes, tag)
			t.publish()
		}
		return
	}
	if old == nil || old.version != p.Version || now.UnixNano() >= old.until.Load() {
		if old != nil {
			old.retire()
		}
		t.generation++
		old = &targetState{version: p.Version, generation: t.generation}
		for i := range old.shards {
			old.shards[i].rows = make(map[targetKey]*panel.TargetRecord)
		}
		old.until.Store(now.Add(time.Duration(p.LeaseSeconds) * time.Second).UnixNano())
		old.enabled.Store(true)
		t.nodes[tag] = old
		t.publish()
	} else {
		old.until.Store(now.Add(time.Duration(p.LeaseSeconds) * time.Second).UnixNano())
	}
}

var targetDomainPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func targetAddress(d M.Socksaddr) (string, string) {
	if d.IsFqdn() {
		if len(d.Fqdn) > 1024 {
			return "", ""
		}
		name, err := idna.Lookup.ToASCII(strings.TrimSuffix(d.Fqdn, "."))
		name = strings.ToLower(name)
		if err != nil || len(name) > 253 || !targetDomainPattern.MatchString(name) {
			return "", ""
		}
		if ip, err := netip.ParseAddr(name); err == nil {
			return ip.Unmap().WithZone("").String(), "ip"
		}
		return name, "domain"
	}
	if d.Addr.IsValid() {
		return d.Addr.Unmap().WithZone("").String(), "ip"
	}
	return "", ""
}
func (t *targetTracker) record(m *meter, d M.Socksaddr, up, down int) {
	if t.active.Load() {
		t.recordActive(m, d, up, down)
	}
}
func (t *targetTracker) recordActive(m *meter, d M.Socksaddr, up, down int) {
	view := t.view.Load()
	if view == nil {
		return
	}
	s := view.nodes[m.tag]
	if s == nil || !s.enabled.Load() {
		return
	}
	shard := &s.shards[(uint64(m.uid)*11400714819323198485)>>(64-5)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if m.targetClosed.Load() || !s.enabled.Load() || time.Now().UnixNano() >= s.until.Load() {
		return
	}
	if m.targetGeneration != s.generation {
		m.targetGeneration = s.generation
		m.targetSeen = nil
		m.targetNames = nil
		m.targetCached = false
		m.targetCounted = false
		m.targetKey = targetKey{}
	}
	var key targetKey
	var cached bool
	if m.targetFixed {
		if !m.targetCached {
			target, kind := targetAddress(d)
			m.targetKey = targetKey{m.uid, kind, target}
			m.targetCached = true
		}
		key = m.targetKey
	} else {
		// Ports are not an aggregation dimension and must not consume the 64-target cap.
		d.Port = 0
		if d.Addr.IsValid() {
			d.Addr = d.Addr.Unmap().WithZone("")
		}
		key, cached = m.targetNames[d]
		if !cached {
			if len(m.targetNames) >= 64 || len(d.Fqdn) > 1024 {
				s.drop(1)
				return
			}
			if s.cacheEntries.Add(1) > targetCacheEntryLimit {
				s.cacheEntries.Add(-1)
				s.drop(1)
				return
			}
			if m.targetNames == nil {
				if s.cacheMeters.Add(1) > targetCacheMeterLimit {
					s.cacheMeters.Add(-1)
					s.cacheEntries.Add(-1)
					s.drop(1)
					return
				}
				m.targetNames = map[M.Socksaddr]targetKey{}
				m.targetSeen = map[targetKey]bool{}
				if shard.meters == nil {
					shard.meters = map[*meter]struct{}{}
				}
				shard.meters[m] = struct{}{}
			}
			target, kind := targetAddress(d)
			key = targetKey{m.uid, kind, target}
			m.targetNames[d] = key
		}
	}
	if key.target == "" {
		return
	}
	row, ok := shard.rows[key]
	if !ok {
		if s.count.Add(1) > targetRowLimit {
			s.count.Add(-1)
			s.drop(1)
			return
		}
		row = &panel.TargetRecord{UserID: m.uid, Target: key.target, Type: key.kind}
		shard.rows[key] = row
	}
	if m.targetFixed {
		if !m.targetCounted {
			m.targetCounted = true
			row.Connections++
		}
	} else if !m.targetSeen[key] {
		m.targetSeen[key] = true
		row.Connections++
	}
	row.UploadBytes += int64(up)
	row.DownloadBytes += int64(down)
}

// Close and policy retirement release caches even when the connection never sends again.
func (t *targetTracker) release(m *meter) {
	m.targetClosed.Store(true)
	view := t.view.Load()
	if view == nil {
		return
	}
	s := view.nodes[m.tag]
	if s == nil {
		return
	}
	shard := &s.shards[(uint64(m.uid)*11400714819323198485)>>(64-5)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if !s.enabled.Load() || m.targetGeneration != s.generation {
		return
	}
	if _, ok := shard.meters[m]; ok {
		s.cacheEntries.Add(-int64(len(m.targetNames)))
		s.cacheMeters.Add(-1)
		delete(shard.meters, m)
		m.targetNames = nil
		m.targetSeen = nil
	}
}

func (v *V2Core) SetTargetPolicy(tag string, p panel.TargetPolicy) { v.tracker.targets.policy(tag, p) }
func (v *V2Core) TargetBatch(tag string) *panel.TargetBatch {
	t := &v.tracker.targets
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.nodes[tag]
	if s == nil {
		return nil
	}
	now := time.Now()
	if now.UnixNano() >= s.until.Load() {
		s.retire()
		delete(t.nodes, tag)
		t.publish()
		return nil
	}
	if s.pending != nil && now.Sub(s.pendingAt) >= targetPendingTTL {
		// Never retry past the panel deduplication horizon (one hour).
		s.drop(int64(len(s.pending.Records)) + s.pending.Dropped)
		s.pending = nil
	}
	if s.pending == nil {
		s.lock()
		defer s.unlock()
		if s.count.Load() == 0 && s.dropped.Load() == 0 {
			return nil
		}
		b := &panel.TargetBatch{Version: s.version, BatchID: uuid.NewString(), Records: make([]panel.TargetRecord, 0, s.count.Load()), Dropped: s.dropped.Swap(0)}
		for i := range s.shards {
			for _, row := range s.shards[i].rows {
				b.Records = append(b.Records, *row)
			}
			clear(s.shards[i].rows)
		}
		s.count.Store(0)
		s.pending = b
		s.pendingAt = now
	}
	return s.pending
}
func (v *V2Core) AckTargets(tag, id string, accepted bool) {
	t := &v.tracker.targets
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.nodes[tag]
	if s == nil || s.pending == nil || s.pending.BatchID != id {
		return
	}
	if !accepted {
		s.retire()
		delete(t.nodes, tag)
		t.publish()
	} else {
		s.pending = nil
	}
}

// Prefer the pre-rewrite address. Never substitute sniffed Domain or reverse DNS.
func originalTarget(destination, routeOriginal, origin M.Socksaddr) M.Socksaddr {
	if origin.IsValid() {
		return origin
	}
	if routeOriginal.IsValid() {
		return routeOriginal
	}
	return destination
}
func packetTarget(m *meter, d M.Socksaddr) M.Socksaddr {
	// A packet destination can change within a UDP session. Preserve a domain
	// only when the session is fixed to it; otherwise use the packet address.
	if m.target.IsFqdn() && (!d.IsValid() || d == m.resolvedTarget) {
		return m.target
	}
	if !d.IsValid() {
		return m.target
	}
	if d.Addr.IsValid() {
		d.Addr = netip.Addr.Unmap(d.Addr)
	}
	return d
}
