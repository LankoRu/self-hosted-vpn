// Package scheduler — периодический сбор статистики, обслуживание БД и
// проверки здоровья сервера с алертами в Telegram.
package scheduler

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/LankoRu/vps-admin/internal/alerts"
	"github.com/LankoRu/vps-admin/internal/bot"
	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/metrics"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/report"
	"github.com/LankoRu/vps-admin/internal/store"
	"github.com/LankoRu/vps-admin/internal/sysinfo"
	"github.com/robfig/cron/v3"
)

// Пороги алертов.
const (
	// collectFailsToAlert — сколько сборов подряд должен упасть сервис, прежде
	// чем писать в Telegram (3 × 5 минут ≈ 15 минут — не дёргаем из-за моргания).
	collectFailsToAlert = 3
	diskAlertPercent    = 90             // занято больше — предупреждаем
	certAlertBefore     = 48 * time.Hour // сертификат истекает раньше — предупреждаем
)

// Start настраивает и запускает cron-задания.
func Start(cfg *config.Config, st *store.Store, reg *registry.Registry, mgr *manager.Manager, b *bot.Bot) {
	// Notifier только при живом боте: typed-nil (*bot.Bot)(nil) в интерфейсе
	// не равен nil, и алерт упал бы на вызове метода.
	var notifier alerts.Notifier
	if b != nil {
		notifier = b
	}
	al := alerts.New(notifier)
	// SkipIfStillRunning: медленный сбор (панель/сервис завис) не должен
	// накладываться на следующий тик — иначе дельты задваиваются.
	c := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)))

	// Сбор статистики каждые 5 минут.
	c.AddFunc("*/5 * * * *", func() { Collect(cfg, st, reg, al) })

	// Проверки здоровья сервера — раз в час.
	c.AddFunc("7 * * * *", func() { CheckHealth(cfg, al) })

	// Ссылки подключения — раз в час: хосты/порты в 3x-ui могли поменяться.
	c.AddFunc("17 * * * *", func() { RefreshLinks(mgr) })

	// Ежедневная очистка старых данных в 04:00.
	c.AddFunc("0 4 * * *", func() {
		log.Println("Плановая очистка старых дельт (>90 дней)")
		st.CleanOldData()
		updateDBSize(st)
	})

	// VACUUM по воскресеньям в 04:30.
	c.AddFunc("30 4 * * 0", func() {
		log.Println("Плановый VACUUM")
		st.Vacuum()
		updateDBSize(st)
	})

	// Еженедельная сводка по трафику в основной чат. Расписание задаёт админ,
	// поэтому ошибку разбора не глотаем — иначе сводка молча не заработает.
	if b != nil && cfg.WeeklyReportEnabled() {
		if _, err := c.AddFunc(cfg.Telegram.WeeklyCron, b.SendWeeklyDigest); err != nil {
			log.Printf("Еженедельная сводка отключена: неверное расписание %q: %v",
				cfg.Telegram.WeeklyCron, err)
		} else {
			log.Printf("Еженедельная сводка: расписание %q (время сервера)", cfg.Telegram.WeeklyCron)
		}
	}

	c.Start()
	log.Println("Планировщик запущен: сбор каждые 5 минут, проверки здоровья — раз в час")

	// Первый сбор сразу при старте, чтобы кэш заполнился.
	go Collect(cfg, st, reg, al)
	go CheckHealth(cfg, al)
	go RefreshLinks(mgr)
}

// RefreshLinks обновляет сохранённые ссылки пользователей; пишет в лог только
// если что-то поменялось или не удалось — чтобы не шуметь каждый час.
func RefreshLinks(mgr *manager.Manager) {
	if updated, failed := mgr.RefreshLinks(); updated > 0 || failed > 0 {
		log.Printf("Ссылки подключения: обновлено %d, ошибок %d", updated, failed)
	}
}

// Collect выполняет один цикл сбора статистики по всем сервисам.
// al может быть nil (тогда алерты просто не шлются).
func Collect(cfg *config.Config, st *store.Store, reg *registry.Registry, al *alerts.Alerter) {
	log.Println("→ сбор статистики")
	for _, coll := range reg.Collectors() {
		start := time.Now()
		traffic, err := coll.GetTraffic()
		metrics.PollDuration.WithLabelValues(coll.Name()).Observe(time.Since(start).Seconds())
		if err != nil {
			log.Printf("[%s] ошибка сбора: %v", coll.Name(), err)
			al.Problem("collect:"+coll.Name(),
				fmt.Sprintf("%s: не отдаёт статистику уже %d проверок подряд\n%v",
					coll.Name(), collectFailsToAlert, err),
				collectFailsToAlert)
			continue
		}
		al.OK("collect:"+coll.Name(), coll.Name()+": сбор статистики восстановлен")
		if len(traffic) > 0 {
			st.UpdateDeltas(coll.Name(), traffic)
		}
	}
	metrics.LastPollTimestamp.Set(float64(time.Now().Unix()))
	updateDBSize(st)
	log.Println("← сбор завершён")
}

// CheckHealth проверяет ресурсы сервера и срок TLS-сертификата.
func CheckHealth(cfg *config.Config, al *alerts.Alerter) {
	// Диск: при переполнении сервис перестанет писать в SQLite.
	if info := sysinfo.Collect(); info.Available && info.DiskTotal > 0 {
		used := int(info.DiskUsed * 100 / info.DiskTotal)
		if used >= diskAlertPercent {
			al.Problem("disk", fmt.Sprintf("Диск заполнен на %d%% (%s из %s) — БД может перестать писаться",
				used, report.HumanBytes(info.DiskUsed), report.HumanBytes(info.DiskTotal)), 1)
		} else {
			al.OK("disk", fmt.Sprintf("Место на диске в норме (занято %d%%)", used))
		}
	}

	// AmneziaWG kernel mode: модуль должен быть загружен. После обновления ядра
	// или ребута DKMS может поставить .ko, но не обновить modules.dep — modprobe
	// молча падает, x-ui пишет WARNING, а UDP-443 не работает (клиенты «не
	// подключаются», и без этой проверки никто не узнает).
	if cfg.Services.AmneziaWG.Enabled && cfg.AmneziaWG3xUI.Kernel {
		if err := checkKernelModule("amneziawg"); err != nil {
			al.Problem("amneziawg", "AmneziaWG (UDP 443) не работает: "+err.Error()+". "+
				"Проверьте: modprobe amneziawg; если 'Module not found' — depmod -a и повторите", 1)
		} else {
			al.OK("amneziawg", "AmneziaWG: модуль ядра загружен")
		}
	}

	// TLS: у Let's Encrypt на IP профиль shortlived — всего ~6 дней. Если cron
	// acme.sh сломается, панель молча станет недоступной в браузерах.
	certFile := cfg.Web.TLS.CertFile
	if !cfg.Web.TLS.Enabled || certFile == "" {
		return
	}
	notAfter, err := certNotAfter(certFile)
	if err != nil {
		al.Problem("cert", "Не удалось прочитать TLS-сертификат "+certFile+": "+err.Error(), 1)
		return
	}
	left := time.Until(notAfter)
	if left < certAlertBefore {
		al.Problem("cert", fmt.Sprintf("TLS-сертификат истекает %s (осталось %dч) — проверьте автопродление acme.sh",
			notAfter.Format("02.01 15:04"), int(left.Hours())), 1)
		return
	}
	al.OK("cert", "TLS-сертификат продлён, действует до "+notAfter.Format("02.01 15:04"))
}

// modulePath — путь к каталогу загруженного модуля (подменяется в тестах).
var modulePath = func(name string) string { return "/sys/module/" + name }

// checkKernelModule проверяет, что модуль ядра загружен.
func checkKernelModule(name string) error {
	if _, err := os.Stat(modulePath(name)); err != nil {
		return fmt.Errorf("модуль %s не загружен", name)
	}
	return nil
}

// certNotAfter возвращает дату окончания первого сертификата в PEM-файле.
func certNotAfter(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for len(raw) > 0 {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		crt, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return crt.NotAfter, nil
	}
	return time.Time{}, fmt.Errorf("в файле нет сертификата")
}

func updateDBSize(st *store.Store) {
	if size, err := st.GetSize(); err == nil {
		metrics.DBSizeBytes.Set(float64(size))
	}
}
