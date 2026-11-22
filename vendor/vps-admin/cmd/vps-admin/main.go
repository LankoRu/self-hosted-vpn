// Command vps-admin — сервис сбора статистики трафика VPN-сервисов
// (xray/3x-ui, AmneziaWG, telemt) с управлением через Telegram-бота.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/LankoRu/vps-admin/internal/bot"
	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/logging"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/report"
	"github.com/LankoRu/vps-admin/internal/scheduler"
	"github.com/LankoRu/vps-admin/internal/singleton"
	"github.com/LankoRu/vps-admin/internal/store"
	"github.com/LankoRu/vps-admin/internal/web"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	cfg := config.ParseFlags()

	closer, err := logging.Setup(cfg.LogPath)
	if err != nil {
		log.Printf("Внимание: логирование в файл недоступно (%v), пишу в stdout", err)
	}
	defer closer.Close()

	log.Println("=== vps-admin запускается ===")
	log.Printf("Конфиг: %s | БД: %s", cfg.ConfigPath, cfg.DBPath)

	st, err := store.New(cfg.DBPath, cfg)
	if err != nil {
		log.Fatalf("Не удалось открыть БД: %v", err)
	}
	defer st.Close()

	reg := registry.New(cfg, st)
	mgr := manager.New(st, reg)

	if cfg.TestMode {
		runTest(cfg, st, reg)
		return
	}

	// Бот необязателен: панель, сбор статистики и алерты-в-лог работают и без
	// него. Нужен, например, для второго сервера под тем же админом или для
	// тестового стенда, где нельзя занимать чужой bot-токен (Telegram разрешает
	// только один поллер на токен — иначе 409 Conflict).
	botEnabled := cfg.Telegram.Token != "" && cfg.Telegram.Token != "YOUR_BOT_TOKEN_HERE"

	// Не даём запуститься второму экземпляру: иначе два поллера Telegram
	// конфликтуют ("Conflict: terminated by other getUpdates request").
	release, err := singleton.Acquire(cfg.DBPath + ".lock")
	if err != nil {
		log.Fatal(err)
	}
	defer release()

	var b *bot.Bot
	if botEnabled {
		b, err = bot.New(cfg, st, mgr)
		if err != nil {
			log.Fatalf("Не удалось запустить Telegram-бота: %v", err)
		}
	} else {
		log.Println("Telegram-бот отключён (telegram.token пуст): панель и сбор работают, уведомления — только в лог")
	}

	if cfg.Prometheus.Enabled {
		go startMetricsServer(cfg.Prometheus.Listen, cfg.Prometheus.Port)
	}
	if cfg.Web.Enabled {
		go web.New(cfg, st, mgr).Start()
	}

	scheduler.Start(cfg, st, reg, mgr, b)

	log.Println("Сервис в рабочем режиме")
	if b != nil {
		b.Start() // блокирует до завершения
		return
	}
	select {} // без бота держим процесс живым
}

func startMetricsServer(host string, port int) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	log.Printf("Prometheus /metrics на %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("Prometheus-сервер остановлен: %v", err)
	}
}

// runTest — одноразовый прогон сбора без запуска бота.
func runTest(cfg *config.Config, st *store.Store, reg *registry.Registry) {
	log.Println("=== ТЕСТОВЫЙ РЕЖИМ: одноразовый сбор ===")
	for _, coll := range reg.Collectors() {
		log.Printf("→ [%s]", coll.Name())
		traffic, err := coll.GetTraffic()
		if err != nil {
			log.Printf("  ошибка: %v", err)
			continue
		}
		var total uint64
		for id, bytes := range traffic {
			total += bytes
			log.Printf("    %-40s %s", id, report.HumanBytes(bytes))
		}
		log.Printf("  итого по %s: %s (%d записей)", coll.Name(), report.HumanBytes(total), len(traffic))
		st.UpdateDeltas(coll.Name(), traffic)
	}
	log.Printf("Размер БД: %s", st.GetSizeHuman())
	log.Println("Тестовый прогон завершён.")
}
