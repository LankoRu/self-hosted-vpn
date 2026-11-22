// Package service описывает контракты сервисов, за которыми следит vps-admin:
// сбор трафика (Collector) и создание/удаление пользователей (Provisioner).
package service

// Collector — сбор статистики трафика с сервиса.
type Collector interface {
	// Name — короткое имя сервиса: xray / amneziawg / telemt.
	Name() string
	// GetTraffic возвращает суммарный трафик (in+out) по каждому идентификатору
	// пользователя внутри сервиса: xray → email, amneziawg → public key,
	// telemt → username.
	GetTraffic() (map[string]uint64, error)
}

// Provisioner — создание и удаление пользователей внутри сервиса.
// Реализуется не всеми сервисами; наличие проверяется type assertion.
type Provisioner interface {
	// Provision создаёт учётку для username и возвращает данные для подключения.
	Provision(username string) (*Account, error)
	// Deprovision удаляет учётку по её идентификатору внутри сервиса.
	Deprovision(identity string) error
}

// LinkRefresher — сервис, у которого ссылки подключения могут измениться на
// стороне сервера без пересоздания учётки (xray: хосты/порты в 3x-ui).
// RefreshItems возвращает актуальные артефакты существующей учётки, НЕ меняя её
// (ключи/UUID те же). Ошибка — «не удалось получить», сохранённое не трогаем.
type LinkRefresher interface {
	RefreshItems(identity string) ([]Item, error)
}

// Account — результат создания пользователя в конкретном сервисе.
type Account struct {
	Service  string `json:"service"`  // имя сервиса
	Identity string `json:"identity"` // ключ пользователя внутри сервиса (email/pubkey/username)
	Address  string `json:"address"`  // выданный адрес (для AmneziaWG), иначе пусто
	Items    []Item `json:"items"`    // артефакты подключения (ссылки/конфиги)
}

// Item — один артефакт подключения: ссылка или конфиг для показа пользователю.
type Item struct {
	Label string `json:"label"`          // подпись варианта
	Value string `json:"value"`          // ссылка или текст конфига/секрета
	QR    bool   `json:"qr,omitempty"`   // показывать QR-код
	File  string `json:"file,omitempty"` // имя файла для скачивания/шаринга (пусто → не файл)
}

// Service объединяет обязательный Collector; провижининг опционален.
type Service interface {
	Collector
}

// RealitySettings — параметры Reality инбаунда 3x-ui. Используются для
// ротации SNI/dest/fingerprint из панели vps-admin («Подключения»).
type RealitySettings struct {
	Dest        string   `json:"dest"`
	ServerNames []string `json:"server_names"`
	Fingerprint string   `json:"fingerprint"`
	PublicKey   string   `json:"public_key,omitempty"`
	ShortIDs    []string `json:"short_ids,omitempty"`
}

// InboundNet — сетевой инбаунд панели для генерации nginx SNI-карты.
type InboundNet struct {
	ID          int      `json:"id"`
	Port        int      `json:"port"`
	Protocol    string   `json:"protocol"`
	Remark      string   `json:"remark"`
	Enabled     bool     `json:"enabled"`
	ServerNames []string `json:"server_names"`
}
