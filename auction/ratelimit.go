package auction

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ipLimiter é um limitador por origem, em memória. Serve para o pedido de
// código de login: o limite por telefone (3 a cada 10 min) não impede um
// atacante de varrer MILHARES de números diferentes e usar o seu número
// comercial como máquina de spam — o que derruba a conta no WhatsApp.
//
// Em memória basta para o caso: é por réplica, e o limite por telefone
// (esse sim no Postgres) continua valendo globalmente.
type ipLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	max    int
}

func newIPLimiter(max int, window time.Duration) *ipLimiter {
	return &ipLimiter{hits: map[string][]time.Time{}, window: window, max: max}
}

func (l *ipLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		l.gcLocked(now) // evita crescer sem limite sob varredura
	}
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}

func (l *ipLimiter) gcLocked(now time.Time) {
	for k, ts := range l.hits {
		alive := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < l.window {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			delete(l.hits, k)
			continue
		}
		l.hits[k] = alive
	}
}

// clientIP prefere o X-Forwarded-For (primeiro valor) quando houver proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return trimSpace(xff[:i])
			}
		}
		return trimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
