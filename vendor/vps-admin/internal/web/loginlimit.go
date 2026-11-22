package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Защита от перебора пароля. Панель обычно слушает внешний адрес, а вход — только
// по одному паролю, поэтому без ограничения попыток его реально подобрать.
const (
	maxLoginFails = 5                // неудач подряд с одного IP до блокировки
	loginBlockFor = 15 * time.Minute // на сколько блокируем IP
	loginFailTTL  = 15 * time.Minute // через сколько «забываем» накопленные неудачи
)

type loginLimiter struct {
	mu   sync.Mutex
	byIP map[string]*loginRecord
}

type loginRecord struct {
	fails     int
	last      time.Time
	blockTill time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{byIP: map[string]*loginRecord{}}
}

// blockedFor — сколько ещё блокирован этот IP (0 = не блокирован).
func (l *loginLimiter) blockedFor(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.byIP[ip]
	if r == nil {
		return 0
	}
	if d := time.Until(r.blockTill); d > 0 {
		return d
	}
	return 0
}

// fail фиксирует неудачную попытку; при превышении лимита включает блокировку.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked()

	r := l.byIP[ip]
	if r == nil {
		r = &loginRecord{}
		l.byIP[ip] = r
	}
	if !r.last.IsZero() && time.Since(r.last) > loginFailTTL {
		r.fails = 0 // прошлые неудачи давно, начинаем окно заново
	}
	r.fails++
	r.last = time.Now()
	if r.fails >= maxLoginFails {
		r.blockTill = time.Now().Add(loginBlockFor)
		r.fails = 0
	}
}

// reset снимает счётчик после удачного входа.
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byIP, ip)
}

// pruneLocked выкидывает протухшие записи, чтобы карта не росла бесконечно.
func (l *loginLimiter) pruneLocked() {
	for ip, r := range l.byIP {
		if time.Since(r.last) > loginFailTTL && time.Until(r.blockTill) <= 0 {
			delete(l.byIP, ip)
		}
	}
}

// clientIP — адрес клиента без порта. X-Forwarded-For намеренно НЕ доверяем:
// панель слушает напрямую, а подделанный заголовок позволил бы обойти лимит.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
