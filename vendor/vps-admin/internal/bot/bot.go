// Package bot — управление сервисом через Telegram: отчёты по трафику и
// регистрация пользователей сразу во всех сервисах.
package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/report"
	"github.com/LankoRu/vps-admin/internal/store"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Bot struct {
	api   *tgbotapi.BotAPI
	cfg   *config.Config
	store *store.Store
	mgr   *manager.Manager
}

// New создаёт бота. store и mgr обязательны.
func New(cfg *config.Config, st *store.Store, mgr *manager.Manager) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return nil, err
	}
	return &Bot{api: api, cfg: cfg, store: st, mgr: mgr}, nil
}

// Notify отправляет служебное сообщение в основной чат (реализует store.Notifier).
func (b *Bot) Notify(text string) {
	if b.cfg.Telegram.ChatID == 0 {
		return
	}
	b.api.Send(tgbotapi.NewMessage(b.cfg.Telegram.ChatID, text))
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	b.api.Send(msg)
}

func (b *Bot) sendDoc(chatID int64, name, content, caption string) {
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{Name: name, Bytes: []byte(content)})
	doc.Caption = caption
	b.api.Send(doc)
}

// Start запускает цикл обработки апдейтов (блокирующий).
func (b *Bot) Start() {
	b.registerCommands()
	log.Printf("Telegram-бот запущен: @%s", b.api.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range b.api.GetUpdatesChan(u) {
		if update.Message == nil {
			continue
		}
		b.handle(update.Message)
	}
}

func (b *Bot) handle(m *tgbotapi.Message) {
	chatID := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	cmd := strings.ToLower(strings.Fields(text + " ")[0])

	switch cmd {
	case "/start", "/help":
		b.send(chatID, b.helpText())
	case "/myid":
		b.send(chatID, fmt.Sprintf("Ваш chat_id: %d", chatID))
	// Отчёты и списки пользователей — только админу: в них имена клиентов,
	// их логины, туннельные адреса и трафик. Иначе любой, кто знает username
	// бота, получил бы всё это, просто написав /listusers.
	case "/size":
		b.adminOnly(chatID, func() { b.send(chatID, "📁 Размер БД: "+b.store.GetSizeHuman()) })

	case "/today":
		b.adminOnly(chatID, func() {
			b.sendReport(chatID, "сегодня", "timestamp >= datetime('now','start of day')")
		})
	case "/week":
		b.adminOnly(chatID, func() {
			b.sendReport(chatID, "неделю", "timestamp >= datetime('now','-7 days')")
		})
	case "/month":
		b.adminOnly(chatID, func() {
			b.sendReport(chatID, "месяц", "timestamp >= datetime('now','start of month')")
		})
	case "/full":
		b.adminOnly(chatID, func() { b.sendReport(chatID, "всё время", "1=1") })
	case "/digest":
		b.adminOnly(chatID, func() { b.sendWeeklyDigest(chatID) })

	case "/listusers":
		b.adminOnly(chatID, func() { b.handleListUsers(chatID) })
	case "/user":
		b.adminOnly(chatID, func() { b.handleUserInfo(chatID, text) })

	case "/register":
		b.adminOnly(chatID, func() { b.handleRegister(chatID, text) })
	case "/device":
		b.adminOnly(chatID, func() { b.handleDevice(chatID, text) })
	case "/deldevice":
		b.adminOnly(chatID, func() { b.handleDelDevice(chatID, text) })
	case "/regen":
		b.adminOnly(chatID, func() { b.handleRegen(chatID, text) })
	case "/regenall":
		b.adminOnly(chatID, func() { b.handleRegenAll(chatID, text) })
	case "/rename":
		b.adminOnly(chatID, func() { b.handleRename(chatID, text) })
	case "/deluser":
		b.adminOnly(chatID, func() { b.handleDelUser(chatID, text) })

	default:
		if strings.HasPrefix(cmd, "/") {
			b.send(chatID, "Неизвестная команда. /help — список команд.")
		}
	}
}

func (b *Bot) adminOnly(chatID int64, fn func()) {
	if !b.cfg.IsAdmin(chatID) {
		b.send(chatID, "⛔ У вас нет прав на эту команду.")
		return
	}
	fn()
}

func (b *Bot) helpText() string {
	return "🤖 vps-admin — мониторинг и управление VPN\n\n" +
		"🔐 Все команды, кроме /start, /help и /myid, доступны только админу.\n\n" +
		"📊 Отчёты:\n" +
		"/today — трафик за сегодня\n" +
		"/week — за 7 дней\n" +
		"/month — за текущий месяц\n" +
		"/full — за всё время\n" +
		"/digest — недельная сводка (та же, что приходит по расписанию)\n" +
		"/size — размер БД\n\n" +
		"👥 Пользователи:\n" +
		"/listusers — список пользователей\n" +
		"/user <имя> — данные и трафик пользователя\n\n" +
		"👤 Управление (только админ):\n" +
		"/register <имя> — создать пользователя во всех сервисах (логин из имени автоматически)\n" +
		"/device <имя> <устройство> — доп. конфиг AmneziaWG для устройства (ноутбук, планшет…)\n" +
		"/deldevice <имя> <устройство> — удалить устройство и его конфиг\n" +
		"/regen <имя> <сервис> — выпустить новый конфиг в одном сервисе\n" +
		"/regenall <сервис> — перевыпустить конфиги всем клиентам сервиса\n" +
		"/rename <имя> <новое отображаемое имя> — сменить подпись пользователя\n" +
		"/deluser <имя> — удалить пользователя из всех сервисов\n" +
		"/myid — ваш chat_id"
}

// ---------------------------------------------------------------------------
// Отчёты
// ---------------------------------------------------------------------------

func (b *Bot) sendReport(chatID int64, period, where string) {
	rows, err := b.store.TrafficByServiceIdentity(where)
	if err != nil {
		log.Printf("отчёт: %v", err)
		b.send(chatID, "❌ Ошибка при построении отчёта.")
		return
	}

	b.send(chatID, report.ServiceSummary(rows, period)+"\n"+report.UserSummary(rows, period))

	full := report.FullUserReport(rows, period)
	b.sendDoc(chatID, "vps_"+sanitize(period)+".txt", full, "Детальная статистика — "+period)
}

// Окна еженедельной сводки: последние 7 дней и предыдущие 7 (для сравнения).
// Скользящее окно, а не календарная неделя: сводка уходит по расписанию, так что
// «последние 7 дней» — это ровно промежуток с прошлой рассылки.
const (
	weeklyWhere    = "timestamp >= datetime('now','-7 days')"
	prevWeekWhere  = "timestamp >= datetime('now','-14 days') AND timestamp < datetime('now','-7 days')"
	weeklyTopUsers = 10
)

// SendWeeklyDigest шлёт еженедельную сводку в основной чат. Вызывается
// планировщиком по расписанию telegram.weekly_cron.
func (b *Bot) SendWeeklyDigest() {
	if b.cfg.Telegram.ChatID == 0 {
		return
	}
	b.sendWeeklyDigest(b.cfg.Telegram.ChatID)
}

func (b *Bot) sendWeeklyDigest(chatID int64) {
	rows, err := b.store.TrafficByServiceIdentity(weeklyWhere)
	if err != nil {
		log.Printf("еженедельная сводка: %v", err)
		b.send(chatID, "❌ Ошибка при построении сводки.")
		return
	}
	// Прошлая неделя нужна только для «+18%» — без неё сводку всё равно шлём.
	prev, err := b.store.TrafficByServiceIdentity(prevWeekWhere)
	if err != nil {
		log.Printf("еженедельная сводка, предыдущая неделя: %v", err)
		prev = nil
	}
	now := time.Now()
	b.send(chatID, report.WeeklyDigest(rows, prev, now.AddDate(0, 0, -7), now, weeklyTopUsers))
}

// ---------------------------------------------------------------------------
// Список / информация о пользователях
// ---------------------------------------------------------------------------

func (b *Bot) handleListUsers(chatID int64) {
	users, err := b.store.ListUsers()
	if err != nil {
		b.send(chatID, "❌ Ошибка при получении списка.")
		return
	}
	if len(users) == 0 {
		b.send(chatID, "Пользователей пока нет. Создайте: /register <имя>")
		return
	}
	lastSeen, _ := b.store.LastSeenByUser()
	var sb strings.Builder
	sb.WriteString("👥 Зарегистрированные пользователи:\n\n")
	for _, u := range users {
		ids, _ := b.store.GetIdentities(u.ID)
		svcs := make([]string, 0, len(ids))
		for _, id := range ids {
			svcs = append(svcs, id.Service)
		}
		fmt.Fprintf(&sb, "• %s — [%s] · %s\n", u.DisplayName, strings.Join(svcs, ", "), ago(lastSeen[u.ID]))
	}
	b.send(chatID, sb.String())
}

// ago переводит время из БД (UTC, 'YYYY-MM-DD HH:MM:SS') в «сколько прошло».
// Сбор идёт раз в 5 минут, поэтому свежее 11 минут считаем активным сейчас.
func ago(ts string) string {
	if ts == "" {
		return "нет активности"
	}
	t, err := time.Parse("2006-01-02 15:04:05", ts)
	if err != nil {
		return "нет активности"
	}
	d := time.Since(t.UTC())
	switch {
	case d < 11*time.Minute:
		return "🟢 активен"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

func (b *Bot) handleUserInfo(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.send(chatID, "Использование: /user <имя>")
		return
	}
	username := parts[1]
	user, err := b.store.GetUserByName(username)
	if err != nil {
		b.send(chatID, "Пользователь не найден.")
		return
	}
	ids, _ := b.store.GetIdentities(user.ID)

	// Трафик за всё время по этому пользователю.
	rows, _ := b.store.TrafficByServiceIdentity("1=1")
	var total uint64
	perSvc := map[string]uint64{}
	for _, r := range rows {
		if r.User == user.DisplayName {
			total += r.Total
			perSvc[r.Service] += r.Total
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "👤 %s (%s)\n\n", user.DisplayName, user.Username)
	sb.WriteString("Идентичности:\n")
	for _, id := range ids {
		addr := ""
		if id.Address != "" {
			addr = " " + id.Address
		}
		fmt.Fprintf(&sb, "  %-10s %s%s\n", id.Service, id.Identity, addr)
	}
	sb.WriteString("\nТрафик за всё время:\n")
	for svc, v := range perSvc {
		fmt.Fprintf(&sb, "  %-10s %s\n", svc, report.HumanBytes(v))
	}
	fmt.Fprintf(&sb, "  Σ Всего: %s\n", report.HumanBytes(total))

	// Активность по каждому сервису: когда последний раз шёл трафик.
	lastSeen, _ := b.store.LastSeenForUser(user.ID)
	sb.WriteString("\nАктивность:\n")
	for _, id := range ids {
		fmt.Fprintf(&sb, "  %-10s %s\n", id.Service, ago(lastSeen[id.Service]))
	}
	b.send(chatID, sb.String())
}

// ---------------------------------------------------------------------------
// Регистрация во всех сервисах
// ---------------------------------------------------------------------------

func (b *Bot) handleRegister(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.send(chatID, "Использование: /register <имя>\nПример: /register Вася Пупкин\nЛогин сгенерируется автоматически (vasya-pupkin).")
		return
	}
	displayName := strings.Join(parts[1:], " ")

	b.send(chatID, fmt.Sprintf("⏳ Создаю %q в сервисах...", displayName))

	user, results, err := b.mgr.RegisterByName(displayName)
	if err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "✅ Пользователь %s создан (логин: %s).\n\n", user.DisplayName, user.Username)
	for _, r := range results {
		if !r.OK {
			fmt.Fprintf(&summary, "❌ %s: %s\n", r.Service, r.Error)
			continue
		}
		fmt.Fprintf(&summary, "✅ %s: готово\n", r.Service)
		// Каждый вариант отдаём отдельно: конфиги-файлы документом, ссылки — текстом.
		for _, it := range r.Account.Items {
			caption := fmt.Sprintf("%s · %s (%s)", r.Service, it.Label, user.DisplayName)
			if it.File != "" {
				b.sendDoc(chatID, it.File, it.Value, caption)
			} else {
				b.send(chatID, fmt.Sprintf("🔑 %s · %s:\n%s", r.Service, it.Label, it.Value))
			}
		}
	}
	b.send(chatID, summary.String())
}

// handleDevice — /device <имя> <устройство>: доп. AWG-конфиг для отдельного
// устройства пользователя. Один WireGuard-ключ нельзя использовать параллельно
// с двух устройств, поэтому на каждое — свой пир; трафик суммируется на юзера.
// Идемпотентно: повторный вызов возвращает существующий конфиг.
func (b *Bot) handleDevice(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "Использование: /device <имя> <устройство>\nПример: /device me ноутбук\nНа каждое устройство — отдельный конфиг AmneziaWG.")
		return
	}
	username := parts[1]
	device := strings.Join(parts[2:], " ")

	b.send(chatID, fmt.Sprintf("⏳ Создаю устройство %q для %q...", device, username))
	acc, err := b.mgr.AddDevice(username, device)
	if err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}
	b.send(chatID, fmt.Sprintf("✅ Устройство %q · %s (%s).\nТрафик всех устройств суммируется на пользователя.", device, username, acc.Identity))
	for _, it := range acc.Items {
		caption := fmt.Sprintf("amneziawg · %s (%s · %s)", it.Label, username, device)
		if it.File != "" {
			b.sendDoc(chatID, it.File, it.Value, caption)
		} else {
			b.send(chatID, fmt.Sprintf("🔑 amneziawg · %s:\n%s", it.Label, it.Value))
		}
	}
}

// handleDelDevice — /deldevice <имя> <устройство>: удаляет учётку устройства
// в панели, identity и её историю трафика. Основную учётку AWG не трогает.
func (b *Bot) handleDelDevice(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "Использование: /deldevice <имя> <устройство>")
		return
	}
	username := parts[1]
	device := strings.Join(parts[2:], " ")
	if err := b.mgr.RemoveDevice(username, device); err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}
	b.send(chatID, fmt.Sprintf("✅ Устройство %q пользователя %q удалено — конфиг больше не работает.", device, username))
}

// handleRegen перевыпускает конфиг пользователя в одном сервисе и присылает
// новые артефакты подключения.
func (b *Bot) handleRegen(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "Использование: /regen <имя> <сервис>")
		return
	}
	username, svc := parts[1], parts[2]

	b.send(chatID, fmt.Sprintf("⏳ Перегенерирую %q в сервисе %s...", username, svc))
	acc, err := b.mgr.Regenerate(username, svc)
	if err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}
	b.send(chatID, fmt.Sprintf("✅ %s · %s: новый конфиг выпущен. Старый больше не работает.", svc, username))
	for _, it := range acc.Items {
		caption := fmt.Sprintf("%s · %s (%s)", svc, it.Label, username)
		if it.File != "" {
			b.sendDoc(chatID, it.File, it.Value, caption)
		} else {
			b.send(chatID, fmt.Sprintf("🔑 %s · %s:\n%s", svc, it.Label, it.Value))
		}
	}
}

// handleRegenAll перевыпускает конфиги всем клиентам сервиса. Конфиги не
// рассылаем (их могут быть десятки) — отдаём сводку; забрать новые можно через
// /user или веб-панель.
func (b *Bot) handleRegenAll(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.send(chatID, "Использование: /regenall <сервис>")
		return
	}
	svc := parts[1]

	b.send(chatID, fmt.Sprintf("⏳ Перегенерирую конфиги всем клиентам %s...", svc))
	results, err := b.mgr.RegenerateAll(svc)
	if err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}
	if len(results) == 0 {
		b.send(chatID, "В этом сервисе нет клиентов для перегенерации.")
		return
	}

	ok := 0
	var sb strings.Builder
	fmt.Fprintf(&sb, "Перегенерация %s:\n\n", svc)
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Fprintf(&sb, "✅ %s\n", r.Username)
		} else {
			fmt.Fprintf(&sb, "❌ %s: %s\n", r.Username, r.Error)
		}
	}
	fmt.Fprintf(&sb, "\nГотово: %d/%d. Новые конфиги — через /user <имя> или веб-панель.", ok, len(results))
	b.send(chatID, sb.String())
}

// handleRename меняет отображаемое имя. Логин остаётся прежним — он же identity
// в сервисах, его смена потребовала бы перевыпуска конфигов.
func (b *Bot) handleRename(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "Использование: /rename <имя> <новое отображаемое имя>")
		return
	}
	username := parts[1]
	display := strings.TrimSpace(strings.Join(parts[2:], " "))
	user, err := b.store.GetUserByName(username)
	if err != nil {
		b.send(chatID, "❌ Пользователь не найден.")
		return
	}
	if err := b.store.SetDisplayName(user.ID, display); err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}
	b.send(chatID, fmt.Sprintf("✅ %s теперь отображается как «%s».", username, display))
}

func (b *Bot) handleDelUser(chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.send(chatID, "Использование: /deluser <имя>")
		return
	}
	username := parts[1]
	results, err := b.mgr.Delete(username)
	if err != nil {
		b.send(chatID, "❌ "+err.Error())
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Удаление %s:\n\n", username)
	for _, r := range results {
		if r.OK {
			fmt.Fprintf(&sb, "✅ %s: удалён\n", r.Service)
		} else {
			fmt.Fprintf(&sb, "⚠️ %s: %s\n", r.Service, r.Error)
		}
	}
	sb.WriteString("\n✅ Пользователь удалён из БД.")
	b.send(chatID, sb.String())
}

func (b *Bot) registerCommands() {
	cmds := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "today", Description: "трафик за сегодня"},
		tgbotapi.BotCommand{Command: "week", Description: "трафик за 7 дней"},
		tgbotapi.BotCommand{Command: "month", Description: "трафик за месяц"},
		tgbotapi.BotCommand{Command: "full", Description: "трафик за всё время"},
		tgbotapi.BotCommand{Command: "digest", Description: "недельная сводка"},
		tgbotapi.BotCommand{Command: "listusers", Description: "список пользователей"},
		tgbotapi.BotCommand{Command: "user", Description: "данные пользователя"},
		tgbotapi.BotCommand{Command: "register", Description: "создать пользователя (админ)"},
		tgbotapi.BotCommand{Command: "regen", Description: "новый конфиг в сервисе (админ)"},
		tgbotapi.BotCommand{Command: "regenall", Description: "перевыпустить всем в сервисе (админ)"},
		tgbotapi.BotCommand{Command: "rename", Description: "сменить отображаемое имя (админ)"},
		tgbotapi.BotCommand{Command: "deluser", Description: "удалить пользователя (админ)"},
		tgbotapi.BotCommand{Command: "size", Description: "размер БД"},
	)
	b.api.Send(cmds)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' {
			return '_'
		}
		return r
	}, s)
}
