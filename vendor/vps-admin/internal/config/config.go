package config

import (
	"flag"
	"os"

	"gopkg.in/yaml.v2"
)

// DefaultConfigPath — путь к конфигу по умолчанию (переопределяется флагом -config).
const DefaultConfigPath = "/etc/vps-admin/config.yaml"

// Config — корневая конфигурация сервиса.
type Config struct {
	DBPath       string `yaml:"db_path"`
	LogPath      string `yaml:"log_path"`
	PollInterval string `yaml:"poll_interval"`

	// Update — обновления компонентов из панели («Система» → «Компоненты»).
	Update struct {
		GithubRepo  string `yaml:"github_repo"`  // репо релизов vps-admin (OWNER/NAME)
		GithubToken string `yaml:"github_token"` // PAT для приватного репо (Contents: Read)
	} `yaml:"update"`

	Telegram struct {
		Token  string  `yaml:"token"`
		ChatID int64   `yaml:"chat_id"` // основной чат для отчётов и алертов
		Admins []int64 `yaml:"admins"`  // дополнительные админы (chat_id)

		// WeeklyReport — слать ли еженедельную сводку в основной чат.
		// Указатель, чтобы отличить «не задано» (nil → включено) от явного false.
		WeeklyReport *bool `yaml:"weekly_report"`
		// WeeklyCron — расписание сводки в формате cron, по локальному времени
		// сервера (по умолчанию понедельник, 10:00).
		WeeklyCron string `yaml:"weekly_cron"`
	} `yaml:"telegram"`

	// Services — включение сбора статистики по каждому сервису.
	Services struct {
		Xray      ServiceToggle `yaml:"xray"`
		AmneziaWG ServiceToggle `yaml:"amneziawg"`
		TeleMT    ServiceToggle `yaml:"telemt"`
	} `yaml:"services"`

	Xray3xUI      Xray3xUIConfig `yaml:"xray_3xui"`
	AmneziaWG3xUI AWG3xUIConfig  `yaml:"amneziawg_3xui"`
	TeleMT        TeleMTConfig   `yaml:"telemt"`

	CounterReset struct {
		ThresholdPercent float64 `yaml:"threshold_percent"`
	} `yaml:"counter_reset"`

	Prometheus struct {
		Port    int  `yaml:"port"`
		Enabled bool `yaml:"enabled"`
	} `yaml:"prometheus"`

	// Web — веб-панель управления пользователями и статистикой.
	Web struct {
		Enabled  bool   `yaml:"enabled"`
		Listen   string `yaml:"listen"`   // адрес:порт (по умолчанию 127.0.0.1:8088)
		Password string `yaml:"password"` // пароль входа
		Login    string `yaml:"login"`    // логин входа (фиксированный, один; нужен браузерным менеджерам паролей)
		TLS      struct {
			Enabled  bool   `yaml:"enabled"`   // включить HTTPS
			CertFile string `yaml:"cert_file"` // свой сертификат; пусто → самоподписанный
			KeyFile  string `yaml:"key_file"`
			Host     string `yaml:"host"` // домен/IP для SAN самоподписанного (через запятую)
		} `yaml:"tls"`
	} `yaml:"web"`

	// Служебные поля, не читаются из YAML.
	ConfigPath string `yaml:"-"`
	TestMode   bool   `yaml:"-"`
}

// ServiceToggle — переключатели для отдельного сервиса.
type ServiceToggle struct {
	Enabled bool `yaml:"enabled"` // собирать статистику
	// Provision != nil → можно регистрировать пользователей.
	// По умолчанию (nil) считается true, если Enabled.
	Provision *bool `yaml:"provision"`
}

// CanProvision — можно ли создавать пользователей в этом сервисе.
func (t ServiceToggle) CanProvision() bool {
	if !t.Enabled {
		return false
	}
	if t.Provision == nil {
		return true
	}
	return *t.Provision
}

// Xray3xUIConfig — доступ к панели 3x-ui и параметры создания клиентов.
// flow не дублируется — берётся из самого inbound при создании клиента.
type Xray3xUIConfig struct {
	BaseURL     string `yaml:"base_url"`
	WebBasePath string `yaml:"web_base_path"`
	APIToken    string `yaml:"api_token"`  // 3x-ui ≥3.0: Settings → Security → API Token
	InboundID   int    `yaml:"inbound_id"` // в какой inbound добавлять клиентов
	SubURL      string `yaml:"sub_url"`    // базовый URL подписки (опционально)
	// PublicHost — публичный адрес (домен/IP) сервера для vless-ссылок. 3x-ui
	// подставляет в ссылку хост из Host-заголовка запроса; т.к. панель мы дёргаем
	// по localhost, без этого в ссылке оказывается localhost. Если пусто —
	// берётся из хоста sub_url. Порт в ссылке всегда из самого inbound.
	PublicHost string `yaml:"public_host"`
	// BackupPort — localhost-порт запасного инбаунда (XHTTP/gRPC), создаётся
	// из раздела «Подключения». Наружу всё равно 443 (nginx SNI-роутер).
	BackupPort int `yaml:"backup_port"`
}

// AWG3xUIConfig — AmneziaWG через инбаунд панели 3x-ui (userspace или kernel
// mode). Ключи, IP и параметры обфускации (AWG 2.0/3.1) не дублируются — их
// генерирует и хранит панель; мы читаем их в рантайме через API
// (clients/get, clients/links).
type AWG3xUIConfig struct {
	BaseURL     string `yaml:"base_url"`      // адрес веб-панели, напр. http://127.0.0.1:2053
	WebBasePath string `yaml:"web_base_path"` // кастомный путь панели (без слэшей по краям)
	APIToken    string `yaml:"api_token"`     // 3x-ui: Settings → Security → API Token
	InboundID   int    `yaml:"inbound_id"`    // ID AmneziaWG-инбаунда
	// PublicHost — публичный адрес (домен/IP) для Endpoint в конфигах. Панель
	// подставляет хост из Host-заголовка запроса; т.к. API дёргается по localhost,
	// без этого в Endpoint окажется localhost. Порт всегда из самого инбаунда.
	PublicHost string `yaml:"public_host"`
}

// TeleMTConfig — MTProto-прокси telemt: сбор статистики и провижининг.
//
// Хост/порт/домен FakeTLS не дублируются — telemt сам генерирует tg://-ссылки
// (dd и ee) из своего config.toml и возвращает их в ответе API. Нужен только
// адрес control-API (см. [server.api].listen в config.toml telemt) и токен.
type TeleMTConfig struct {
	BaseURL  string `yaml:"base_url"`  // напр. http://127.0.0.1:9091
	AdminKey string `yaml:"admin_key"` // [server.api].auth_header, если задан
	// ConfigPath — config.toml telemt: чтение/смена домена-маски (tls_domain,
	// mask_host) и секретов пользователей для RefreshItems.
	ConfigPath string `yaml:"config_path"`
}

// Load читает конфигурацию из YAML-файла.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.ConfigPath = path
	return &cfg, nil
}

// ParseFlags разбирает аргументы командной строки и загружает конфиг.
func ParseFlags() *Config {
	path := flag.String("config", DefaultConfigPath, "path to config.yaml")
	test := flag.Bool("test", false, "одноразовый прогон сбора статистики и выход")
	flag.Parse()

	cfg, err := Load(*path)
	if err != nil {
		// Конфиг не найден — работаем на дефолтах, ошибку залогирует main.
		cfg = &Config{ConfigPath: *path}
	}

	cfg.TestMode = *test
	cfg.SetDefaults()
	return cfg
}

// SetDefaults проставляет значения по умолчанию.
func (c *Config) SetDefaults() {
	if c.DBPath == "" {
		c.DBPath = "/var/lib/vps-admin/stats.db"
	}
	if c.Xray3xUI.BackupPort == 0 {
		c.Xray3xUI.BackupPort = 10092
	}
	if c.TeleMT.ConfigPath == "" {
		c.TeleMT.ConfigPath = "/etc/telemt/telemt.toml"
	}
	if c.Update.GithubRepo == "" {
		c.Update.GithubRepo = "LankoRu/self-hosted-vpn"
	}
	if c.LogPath == "" {
		c.LogPath = "/var/log/vps-admin/vps-admin.log"
	}
	if c.PollInterval == "" {
		c.PollInterval = "5m"
	}
	if c.CounterReset.ThresholdPercent <= 0 {
		c.CounterReset.ThresholdPercent = 5.0
	}
	if c.Prometheus.Port == 0 {
		c.Prometheus.Port = 2112
	}
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8088"
	}
	if c.Web.Login == "" {
		c.Web.Login = "admi"
	}
	if c.Telegram.WeeklyCron == "" {
		c.Telegram.WeeklyCron = "0 10 * * 1" // понедельник, 10:00
	}
	// Если ни один сервис явно не включён — включаем все.
	if !c.Services.Xray.Enabled && !c.Services.AmneziaWG.Enabled && !c.Services.TeleMT.Enabled {
		c.Services.Xray.Enabled = true
		c.Services.AmneziaWG.Enabled = true
		c.Services.TeleMT.Enabled = true
	}
}

// WeeklyReportEnabled — слать ли еженедельную сводку по трафику.
// Поля нет в конфиге → считаем включённой.
func (c *Config) WeeklyReportEnabled() bool {
	return c.Telegram.WeeklyReport == nil || *c.Telegram.WeeklyReport
}

// IsAdmin проверяет, является ли chat_id администратором.
func (c *Config) IsAdmin(chatID int64) bool {
	if chatID == c.Telegram.ChatID {
		return true
	}
	for _, id := range c.Telegram.Admins {
		if id == chatID {
			return true
		}
	}
	return false
}
