package naive

import (
	"crypto/subtle"
	E "github.com/sagernet/sing/common/exceptions"
	"net"
	"net/http"
	"time"
)

func (n *Inbound) releaseTransport(conn net.Conn) {
	if value, ok := n.connections.LoadAndDelete(conn); ok {
		state := value.(*transportState)
		state.mu.Lock()
		state.closed = true
		if state.idleTimer != nil {
			state.idleTimer.Stop()
		}
		release := state.release
		state.mu.Unlock()
		if release != nil {
			release()
		}
	}
}

func (s *transportState) setIdle(idle bool, timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	if !idle || s.closed {
		return
	}
	// Compare timer identity as Stop cannot prevent an already queued callback.
	var timer *time.Timer
	timer = time.AfterFunc(timeout, func() {
		s.mu.Lock()
		if s.closed || s.idleTimer != timer {
			s.mu.Unlock()
			return
		}
		s.closed = true
		s.mu.Unlock()
		_ = s.conn.Close()
	})
	s.idleTimer = timer
}

// Publish complete immutable credential tables; empty tables deny everyone.
func (n *Inbound) UpdateUsers(users []User) error {
	table := make(map[string]User, len(users))
	identities := make([]string, 0, len(users))
	for _, u := range users {
		if u.Name == "" || u.Username == "" || u.Password == "" {
			return E.New("invalid Naive user")
		}
		if _, exists := table[u.Username]; exists {
			return E.New("duplicate Naive username")
		}
		table[u.Username] = u
		identities = append(identities, u.Name)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.transports.Replace(identities)
	n.users = table
	return nil
}

func (n *Inbound) authenticate(r *http.Request, username, password string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	user, ok := n.users[username]
	if !ok || subtle.ConstantTimeCompare([]byte(user.Password), []byte(password)) != 1 {
		return "", false
	}
	state, ok := r.Context().Value(transportKey{}).(*transportState)
	if !ok {
		return "", false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || (state.identity != "" && state.identity != user.Name) {
		return "", false
	}
	if state.identity == "" {
		release, admitted := n.transports.Track(user.Name, func() { _ = state.conn.Close() })
		if !admitted {
			return "", false
		}
		state.identity, state.release = user.Name, release
	}
	return user.Name, true
}
