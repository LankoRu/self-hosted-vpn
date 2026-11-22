// Package alerts — уведомления о проблемах сервера в Telegram с защитой от
// повторов: сообщение уходит один раз при поломке и один раз при восстановлении.
// Без этого сбор раз в 5 минут заваливал бы чат одинаковыми алертами.
package alerts

import (
	"log"
	"sync"
)

// Notifier — получатель уведомлений (реализует бот).
type Notifier interface {
	Notify(text string)
}

// Alerter хранит состояние по каждой проверке (ключ = что проверяем).
type Alerter struct {
	mu     sync.Mutex
	n      Notifier
	fails  map[string]int  // сколько проверок подряд провалено
	firing map[string]bool // по этому ключу алерт уже отправлен
}

func New(n Notifier) *Alerter {
	return &Alerter{n: n, fails: map[string]int{}, firing: map[string]bool{}}
}

// Problem фиксирует неудачную проверку. Уведомление уходит, когда неудач подряд
// стало >= threshold и алерт по этому ключу ещё не отправлялся.
func (a *Alerter) Problem(key, msg string, threshold int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.fails[key]++
	fails := a.fails[key]
	fire := fails >= threshold && !a.firing[key]
	if fire {
		a.firing[key] = true
	}
	a.mu.Unlock()

	if fire {
		log.Printf("[alert] %s: %s", key, msg)
		a.notify("🔴 " + msg)
	}
}

// OK фиксирует успешную проверку; если по ключу висел алерт — шлёт «восстановлено».
func (a *Alerter) OK(key, recoveredMsg string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.fails[key] = 0
	wasFiring := a.firing[key]
	delete(a.firing, key)
	a.mu.Unlock()

	if wasFiring {
		log.Printf("[alert] %s: восстановлено", key)
		a.notify("🟢 " + recoveredMsg)
	}
}

func (a *Alerter) notify(text string) {
	if a.n != nil {
		a.n.Notify(text)
	}
}
