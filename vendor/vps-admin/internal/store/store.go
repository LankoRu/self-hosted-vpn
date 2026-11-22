// Package store — слой доступа к SQLite: пользователи, их идентичности в
// сервисах, кэш и дельты трафика.
package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/metrics"
	"github.com/LankoRu/vps-admin/internal/report"
	_ "modernc.org/sqlite"
)

// Store — обёртка над *sql.DB.
type Store struct {
	*sql.DB
	Path string
	cfg  *config.Config
}

// User — каноничный пользователь, единый для всех сервисов.
type User struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	TelegramID  int64     `json:"telegram_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Identity — идентичность пользователя внутри конкретного сервиса.
type Identity struct {
	UserID   int64  `json:"user_id"`
	Service  string `json:"service"`
	Identity string `json:"identity"`
	Address  string `json:"address"`
	Config   string `json:"config"`
}

// New открывает БД и инициализирует схему.
func New(path string, cfg *config.Config) (*Store, error) {
	if dir := dirOf(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть БД %s: %w", path, err)
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			log.Printf("Warning: %s: %v", pragma, err)
		}
	}

	s := &Store{DB: db, Path: path, cfg: cfg}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	log.Printf("База данных инициализирована: %s", path)
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id           INTEGER PRIMARY KEY,
			username     TEXT UNIQUE NOT NULL,
			display_name TEXT,
			telegram_id  INTEGER,
			created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS identities (
			id         INTEGER PRIMARY KEY,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			service    TEXT NOT NULL,
			identity   TEXT NOT NULL,
			address    TEXT,
			config     TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(service, identity)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_identities_user ON identities(user_id)`,
		`CREATE TABLE IF NOT EXISTS stats_cache (
			service        TEXT,
			identity       TEXT,
			last_total     INTEGER,
			last_poll_time DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (service, identity)
		)`,
		`CREATE TABLE IF NOT EXISTS traffic_deltas (
			id          INTEGER PRIMARY KEY,
			service     TEXT,
			identity    TEXT,
			timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP,
			total_bytes INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_deltas_ts ON traffic_deltas(timestamp)`,
		// Публичная ссылка на страницу с ключами пользователя. Одна на пользователя
		// (PRIMARY KEY по user_id), живёт до отзыва, удаляется вместе с пользователем.
		`CREATE TABLE IF NOT EXISTS share_links (
			user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			token      TEXT UNIQUE NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_share_token ON share_links(token)`,
	}
	for _, q := range stmts {
		if _, err := s.Exec(q); err != nil {
			return fmt.Errorf("миграция не удалась (%.40s...): %w", q, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Пользователи и идентичности
// ---------------------------------------------------------------------------

// CreateUser создаёт каноничного пользователя. Возвращает ошибку, если имя занято.
func (s *Store) CreateUser(username, displayName string, telegramID int64) (int64, error) {
	if displayName == "" {
		displayName = username
	}
	res, err := s.Exec(
		`INSERT INTO users (username, display_name, telegram_id) VALUES (?,?,?)`,
		username, displayName, telegramID)
	if err != nil {
		return 0, err
	}
	s.refreshUserCount()
	return res.LastInsertId()
}

// GetUserByName возвращает пользователя по имени.
func (s *Store) GetUserByName(username string) (*User, error) {
	row := s.QueryRow(
		`SELECT id, username, display_name, COALESCE(telegram_id,0), created_at
		 FROM users WHERE username = ?`, username)
	return scanUser(row)
}

// ListUsers возвращает всех пользователей, отсортированных по имени.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.Query(
		`SELECT id, username, display_name, COALESCE(telegram_id,0), created_at
		 FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

// SetDisplayName меняет отображаемое имя пользователя. Логин (username) не
// трогаем: он же identity во всех сервисах и ключ traffic_deltas — его смена
// потребовала бы перевыпуска конфигов и отвязала бы историю трафика.
func (s *Store) SetDisplayName(id int64, displayName string) error {
	_, err := s.Exec(`UPDATE users SET display_name = ? WHERE id = ?`, displayName, id)
	return err
}

// DeleteUser удаляет пользователя и (каскадно) его идентичности.
func (s *Store) DeleteUser(id int64) error {
	_, err := s.Exec(`DELETE FROM users WHERE id = ?`, id)
	s.refreshUserCount()
	return err
}

// ---------------------------------------------------------------------------
// Публичные ссылки на страницу с ключами
// ---------------------------------------------------------------------------

// SetShareToken создаёт (или заменяет) публичную ссылку пользователя.
func (s *Store) SetShareToken(userID int64, token string) error {
	_, err := s.Exec(
		`INSERT OR REPLACE INTO share_links (user_id, token) VALUES (?,?)`, userID, token)
	return err
}

// GetShareToken возвращает токен публичной ссылки пользователя ("" — ссылки нет).
func (s *Store) GetShareToken(userID int64) (string, error) {
	var token string
	err := s.QueryRow(`SELECT token FROM share_links WHERE user_id = ?`, userID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return token, err
}

// DeleteShareToken отзывает публичную ссылку пользователя.
func (s *Store) DeleteShareToken(userID int64) error {
	_, err := s.Exec(`DELETE FROM share_links WHERE user_id = ?`, userID)
	return err
}

// GetUserByShareToken находит пользователя по токену публичной ссылки.
func (s *Store) GetUserByShareToken(token string) (*User, error) {
	row := s.QueryRow(
		`SELECT u.id, u.username, u.display_name, COALESCE(u.telegram_id,0), u.created_at
		 FROM users u JOIN share_links sl ON sl.user_id = u.id
		 WHERE sl.token = ?`, token)
	return scanUser(row)
}

// AddIdentity привязывает идентичность сервиса к пользователю.
func (s *Store) AddIdentity(userID int64, service, identity, address, cfg string) error {
	_, err := s.Exec(
		`INSERT OR REPLACE INTO identities (user_id, service, identity, address, config)
		 VALUES (?,?,?,?,?)`,
		userID, service, identity, address, cfg)
	return err
}

// SetIdentityConfig обновляет config идентичности. Возвращает true, только если
// значение действительно поменялось.
func (s *Store) SetIdentityConfig(service, identity, cfg string) (bool, error) {
	res, err := s.Exec(
		`UPDATE identities SET config = ?
		 WHERE service = ? AND identity = ? AND config IS NOT ?`,
		cfg, service, identity, cfg)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetIdentities возвращает все идентичности пользователя.
func (s *Store) GetIdentities(userID int64) ([]Identity, error) {
	// length(identity): основная учётка сервиса короче её устройств
	// (<user>-awg < <user>-awg-<device>) — Regenerate берёт первую и должен
	// перевыпускать именно основную, а не устройство. Порядок стабилен при regen.
	return s.queryIdentities(`WHERE user_id = ? ORDER BY service, length(identity), identity`, userID)
}

// DeleteIdentity удаляет одну идентичность (устройство AWG). Каскад на users
// не влияет — строка удаляется точечно.
func (s *Store) DeleteIdentity(service, identity string) error {
	_, err := s.Exec(`DELETE FROM identities WHERE service = ? AND identity = ?`, service, identity)
	return err
}

// DeleteIdentityTraffic стирает историю трафика идентичности (дельты + кэш).
// Вызывается при удалении устройства, чтобы его трафик не повис «не привязанным»
// в overview (LEFT JOIN identities) и не расходился с суммой по пользователям.
func (s *Store) DeleteIdentityTraffic(service, identity string) error {
	if _, err := s.Exec(`DELETE FROM traffic_deltas WHERE service = ? AND identity = ?`, service, identity); err != nil {
		return err
	}
	_, err := s.Exec(`DELETE FROM stats_cache WHERE service = ? AND identity = ?`, service, identity)
	return err
}

// IdentitiesByService возвращает все идентичности одного сервиса.
func (s *Store) IdentitiesByService(service string) ([]Identity, error) {
	return s.queryIdentities(`WHERE service = ? ORDER BY identity`, service)
}

func (s *Store) queryIdentities(where string, args ...interface{}) ([]Identity, error) {
	rows, err := s.Query(
		`SELECT user_id, service, identity, COALESCE(address,''), COALESCE(config,'')
		 FROM identities `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.UserID, &id.Service, &id.Identity, &id.Address, &id.Config); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UsedAddresses возвращает множество занятых адресов внутри сервиса
// (используется для выделения IP пирам AmneziaWG).
func (s *Store) UsedAddresses(service string) (map[string]bool, error) {
	rows, err := s.Query(
		`SELECT address FROM identities WHERE service = ? AND address != ''`, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	used := make(map[string]bool)
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		used[a] = true
	}
	return used, rows.Err()
}

// DisplayFor возвращает человекочитаемое имя для идентичности сервиса
// (имя пользователя, если идентичность привязана, иначе сам идентификатор).
func (s *Store) DisplayFor(service, identity string) string {
	var name string
	err := s.QueryRow(
		`SELECT u.display_name FROM identities i
		 JOIN users u ON u.id = i.user_id
		 WHERE i.service = ? AND i.identity = ?`, service, identity).Scan(&name)
	if err != nil || name == "" {
		return identity
	}
	return name
}

func (s *Store) refreshUserCount() {
	var n int
	if err := s.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err == nil {
		metrics.UsersRegistered.Set(float64(n))
	}
}

// ---------------------------------------------------------------------------
// Трафик: дельты и защита от сброса счётчиков
// ---------------------------------------------------------------------------

// UpdateDeltas обновляет накопленную статистику по сервису с защитой от
// сброса счётчиков (перезапуск сервиса обнуляет счётчик — не считаем это трафиком).
func (s *Store) UpdateDeltas(service string, traffic map[string]uint64) {
	now := time.Now().Format("2006-01-02 15:04:05")

	for identity, current := range traffic {
		var lastTotal sql.NullInt64
		_ = s.QueryRow(
			`SELECT last_total FROM stats_cache WHERE service=? AND identity=?`,
			service, identity).Scan(&lastTotal)

		delta := current

		if lastTotal.Valid {
			last := uint64(lastTotal.Int64)
			switch {
			case current >= last:
				delta = current - last
			default:
				// Счётчик уменьшился — возможен сброс.
				dropPercent := float64(last-current) / float64(last) * 100
				if dropPercent > s.cfg.CounterReset.ThresholdPercent || current == 0 {
					delta = current
					metrics.CounterResetDetected.WithLabelValues(service, identity).Inc()
					log.Printf("⚠️ Сброс счётчиков | %s | %s | -%.2f%%", service, identity, dropPercent)
				} else {
					delta = 0
				}
			}
		}

		if delta > 0 {
			_, _ = s.Exec(
				`INSERT INTO traffic_deltas (service, identity, total_bytes) VALUES (?,?,?)`,
				service, identity, delta)
			metrics.TrafficTotal.WithLabelValues(service, identity).Add(float64(delta))
		}

		_, _ = s.Exec(
			`INSERT OR REPLACE INTO stats_cache (service, identity, last_total, last_poll_time)
			 VALUES (?,?,?,?)`, service, identity, current, now)
	}
}

// TrafficByServiceIdentity возвращает агрегированный трафик по (сервис,
// идентичность) за период; идентичности сопоставляются с каноничными
// пользователями (username пуст, если идентичность не зарегистрирована).
func (s *Store) TrafficByServiceIdentity(where string, args ...interface{}) ([]report.Row, error) {
	q := `SELECT td.service,
	             COALESCE(u.display_name, ''),
	             td.identity,
	             SUM(td.total_bytes)
	      FROM traffic_deltas td
	      LEFT JOIN identities i ON i.service = td.service AND i.identity = td.identity
	      LEFT JOIN users u ON u.id = i.user_id
	      WHERE ` + where + `
	      GROUP BY td.service, u.id, td.identity
	      ORDER BY SUM(td.total_bytes) DESC`

	rows, err := s.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []report.Row
	for rows.Next() {
		var r report.Row
		if err := rows.Scan(&r.Service, &r.User, &r.Identity, &r.Total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrafficByUser возвращает суммарный трафик по каждому user_id за период
// (where — константное SQL-условие по столбцу timestamp, напр. "1=1").
func (s *Store) TrafficByUser(where string) (map[int64]uint64, error) {
	q := `SELECT i.user_id, SUM(td.total_bytes)
	      FROM traffic_deltas td
	      JOIN identities i ON i.service = td.service AND i.identity = td.identity
	      WHERE ` + where + `
	      GROUP BY i.user_id`
	rows, err := s.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]uint64)
	for rows.Next() {
		var uid int64
		var total uint64
		if err := rows.Scan(&uid, &total); err != nil {
			return nil, err
		}
		out[uid] = total
	}
	return out, rows.Err()
}

// TrafficForUser возвращает трафик одного пользователя по сервисам за период.
func (s *Store) TrafficForUser(userID int64, where string) (map[string]uint64, error) {
	q := `SELECT td.service, SUM(td.total_bytes)
	      FROM traffic_deltas td
	      JOIN identities i ON i.service = td.service AND i.identity = td.identity
	      WHERE i.user_id = ? AND (` + where + `)
	      GROUP BY td.service`
	rows, err := s.Query(q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]uint64)
	for rows.Next() {
		var svc string
		var total uint64
		if err := rows.Scan(&svc, &total); err != nil {
			return nil, err
		}
		out[svc] = total
	}
	return out, rows.Err()
}

// Активность = момент последнего замера с НЕнулевым трафиком. Нулевые дельты
// пишутся каждые 5 минут просто по факту сбора и активностью не являются.

// LastSeenByUser возвращает время последней активности по каждому user_id
// (UTC, 'YYYY-MM-DD HH:MM:SS'). Пользователи без трафика в карте отсутствуют.
func (s *Store) LastSeenByUser() (map[int64]string, error) {
	rows, err := s.Query(
		`SELECT i.user_id, MAX(td.timestamp)
		 FROM traffic_deltas td
		 JOIN identities i ON i.service = td.service AND i.identity = td.identity
		 WHERE td.total_bytes > 0
		 GROUP BY i.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]string)
	for rows.Next() {
		var uid int64
		var ts string
		if err := rows.Scan(&uid, &ts); err != nil {
			return nil, err
		}
		out[uid] = ts
	}
	return out, rows.Err()
}

// LastSeenForUser возвращает время последней активности одного пользователя
// по каждому сервису (UTC). Сервисы без трафика в карте отсутствуют.
func (s *Store) LastSeenForUser(userID int64) (map[string]string, error) {
	rows, err := s.Query(
		`SELECT td.service, MAX(td.timestamp)
		 FROM traffic_deltas td
		 JOIN identities i ON i.service = td.service AND i.identity = td.identity
		 WHERE i.user_id = ? AND td.total_bytes > 0
		 GROUP BY td.service`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var svc, ts string
		if err := rows.Scan(&svc, &ts); err != nil {
			return nil, err
		}
		out[svc] = ts
	}
	return out, rows.Err()
}

// SeriesPoint — одна точка ряда: временной бакет, ключ серии и сумма байт.
type SeriesPoint struct {
	Bucket string
	Key    string
	Total  uint64
}

// TrafficSeries агрегирует трафик по времени (bucketExpr) и произвольному ключу
// серии (keyExpr). ВАЖНО: bucketExpr, keyExpr и where — КОНСТАНТНЫЙ SQL из кода
// (не пользовательский ввод), поэтому подставляются в запрос напрямую. Джойны к
// identities/users включены всегда, чтобы keyExpr мог ссылаться на пользователя.
// Для «одного бакета» (итоги без времени) передайте bucketExpr = "'all'".
func (s *Store) TrafficSeries(bucketExpr, keyExpr, where string) ([]SeriesPoint, error) {
	q := `SELECT ` + bucketExpr + ` AS b, ` + keyExpr + ` AS k, SUM(td.total_bytes)
	      FROM traffic_deltas td
	      LEFT JOIN identities i ON i.service = td.service AND i.identity = td.identity
	      LEFT JOIN users u ON u.id = i.user_id
	      WHERE ` + where + `
	      GROUP BY b, k
	      ORDER BY b`
	rows, err := s.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Key, &p.Total); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Обслуживание БД
// ---------------------------------------------------------------------------

func (s *Store) CleanOldData() {
	if _, err := s.Exec(
		`DELETE FROM traffic_deltas WHERE timestamp < datetime('now', '-90 days')`); err != nil {
		log.Printf("CleanOldData error: %v", err)
	}
}

func (s *Store) Vacuum() {
	before, _ := s.GetSize()
	if _, err := s.Exec("VACUUM"); err != nil {
		log.Printf("VACUUM error: %v", err)
		return
	}
	_, _ = s.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	after, _ := s.GetSize()
	log.Printf("VACUUM выполнен | До: %s → После: %s",
		report.HumanBytes(before), report.HumanBytes(after))
}

func (s *Store) ResetCache() error {
	_, err := s.Exec("DELETE FROM stats_cache")
	return err
}

func (s *Store) GetSize() (uint64, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return uint64(info.Size()), nil
}

func (s *Store) GetSizeHuman() string {
	size, err := s.GetSize()
	if err != nil {
		return "ошибка"
	}
	if size == 0 {
		return "0 B"
	}
	return report.HumanBytes(size)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...interface{}) error
}

func scanUser(row scanner) (*User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.TelegramID, &created); err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", created)
	return &u, nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}
