package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// TrafficTotal — суммарный трафик по сервису и идентификатору.
	TrafficTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vps_traffic_bytes_total",
			Help: "Total traffic bytes (in+out) by service and identity",
		},
		[]string{"service", "identity"},
	)

	// PollDuration — длительность сбора статистики по каждому сервису.
	PollDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vps_poll_duration_seconds",
			Help:    "Duration of traffic collection per service",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"service"},
	)

	// DBSizeBytes — текущий размер файла БД.
	DBSizeBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vps_db_size_bytes",
		Help: "Current size of SQLite database file",
	})

	// LastPollTimestamp — время последнего успешного цикла сбора.
	LastPollTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vps_last_poll_timestamp",
		Help: "Unix timestamp of last successful poll",
	})

	// CounterResetDetected — обнаруженные сбросы счётчиков.
	CounterResetDetected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vps_counter_reset_detected_total",
			Help: "Number of detected counter resets",
		},
		[]string{"service", "identity"},
	)

	// UsersRegistered — количество зарегистрированных пользователей.
	UsersRegistered = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vps_users_registered",
		Help: "Number of registered canonical users",
	})
)
