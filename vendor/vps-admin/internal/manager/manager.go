// Package manager — общая бизнес-логика управления пользователями
// (создание/удаление во всех сервисах), используемая и ботом, и веб-интерфейсом.
package manager

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/store"
	"github.com/LankoRu/vps-admin/internal/util"
)

// usernameRe — допустимые имена (становятся email в xray, username в telemt,
// именем файла конфига AmneziaWG).
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{2,40}$`)

// Manager инкапсулирует store и реестр сервисов.
type Manager struct {
	store *store.Store
	reg   *registry.Registry
}

func New(st *store.Store, reg *registry.Registry) *Manager {
	return &Manager{store: st, reg: reg}
}

// ServiceResult — итог провижининга/депровижининга в одном сервисе.
type ServiceResult struct {
	Service string           `json:"service"`
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	Account *service.Account `json:"account,omitempty"`
}

// RegisterByName создаёт пользователя, задавая ТОЛЬКО отображаемое имя:
// логин генерируется из него транслитом (util.UsernameFromDisplay), при
// коллизии добавляется числовой суффикс (-2, -3, …). Логин — identity во
// всех сервисах и ключ истории трафика, поэтому уникальность обязательна.
func (m *Manager) RegisterByName(displayName string) (*store.User, []ServiceResult, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, nil, fmt.Errorf("введите имя пользователя")
	}
	base := util.UsernameFromDisplay(displayName)
	username := base
	for i := 2; ; i++ {
		if _, err := m.store.GetUserByName(username); err != nil {
			break // свободно
		}
		username = fmt.Sprintf("%s-%d", base, i)
	}
	return m.Register(username, displayName)
}

// Register создаёт пользователя во всех сервисах, поддерживающих провижининг.
// Возвращает созданного пользователя и результат по каждому сервису.
func (m *Manager) Register(username, displayName string) (*store.User, []ServiceResult, error) {
	if !usernameRe.MatchString(username) {
		return nil, nil, fmt.Errorf("недопустимое имя: разрешены латиница, цифры, . _ - (2–40 символов)")
	}
	if _, err := m.store.GetUserByName(username); err == nil {
		return nil, nil, fmt.Errorf("пользователь %q уже существует", username)
	}
	if displayName == "" {
		displayName = username
	}

	userID, err := m.store.CreateUser(username, displayName, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось создать пользователя: %w", err)
	}

	var results []ServiceResult
	created := 0
	for name, p := range m.reg.Provisioners() {
		acc, err := p.Provision(username)
		if err != nil {
			results = append(results, ServiceResult{Service: name, OK: false, Error: err.Error()})
			continue
		}
		itemsJSON, _ := json.Marshal(acc.Items)
		if err := m.store.AddIdentity(userID, acc.Service, acc.Identity, acc.Address, string(itemsJSON)); err != nil {
			// Учётка в сервисе уже создана — убираем её, иначе она останется
			// сиротой: трафик «не привязан», а логин нельзя пересоздать.
			if derr := p.Deprovision(acc.Identity); derr != nil {
				log.Printf("[manager] %s/%s: откат после ошибки БД не удался: %v", name, acc.Identity, derr)
			}
			results = append(results, ServiceResult{Service: name, OK: false, Error: "identity: " + err.Error()})
			continue
		}
		created++
		results = append(results, ServiceResult{Service: name, OK: true, Account: acc})
	}

	if created == 0 {
		_ = m.store.DeleteUser(userID)
		return nil, results, fmt.Errorf("ни один сервис не создан — пользователь удалён")
	}

	user, _ := m.store.GetUserByName(username)
	return user, results, nil
}

// DeviceProvisioner — опциональный интерфейс адаптера для multi-device.
// Реализует awg3xui: отдельный клиент на каждое устройство пользователя.
type DeviceProvisioner interface {
	ProvisionDevice(username, device string) (*service.Account, error)
}

// AddDevice создаёт доп. AWG-конфиг для устройства пользователя (ноутбук,
// планшет…): учётка `<username>-awg-<slug>` привязывается к ТОМУ ЖЕ юзеру —
// трафик всех устройств суммируется. Идемпотентно: повторный вызов с тем же
// именем устройства возвращает сохранённый конфиг без пересоздания.
func (m *Manager) AddDevice(username, device string) (*service.Account, error) {
	user, err := m.store.GetUserByName(username)
	if err != nil {
		return nil, fmt.Errorf("пользователь не найден")
	}
	device = strings.TrimSpace(device)
	if device == "" {
		return nil, fmt.Errorf("введите название устройства")
	}
	slug := util.UsernameFromDisplay(device)
	identity := username + "-awg-" + slug

	ids, _ := m.store.GetIdentities(user.ID)
	for _, id := range ids {
		if id.Service == "amneziawg" && id.Identity == identity {
			var items []service.Item
			_ = json.Unmarshal([]byte(id.Config), &items)
			return &service.Account{Service: id.Service, Identity: id.Identity, Address: id.Address, Items: items}, nil
		}
	}

	p, ok := m.reg.Provisioners()["amneziawg"]
	if !ok {
		return nil, fmt.Errorf("сервис amneziawg не включён")
	}
	dp, ok := p.(DeviceProvisioner)
	if !ok {
		return nil, fmt.Errorf("сервис amneziawg не поддерживает устройства")
	}
	acc, err := dp.ProvisionDevice(username, slug)
	if err != nil {
		return nil, fmt.Errorf("не удалось создать устройство: %w", err)
	}
	itemsJSON, _ := json.Marshal(acc.Items)
	if err := m.store.AddIdentity(user.ID, acc.Service, acc.Identity, acc.Address, string(itemsJSON)); err != nil {
		if derr := p.Deprovision(acc.Identity); derr != nil {
			log.Printf("[manager] amneziawg/%s: откат после ошибки БД не удался: %v", acc.Identity, derr)
		}
		return nil, fmt.Errorf("устройство не сохранено в БД (учётка откатена): %w", err)
	}
	log.Printf("[manager] amneziawg: устройство %q для %q создано (%s)", device, username, acc.Identity)
	return acc, nil
}

// RemoveDevice удаляет устройство пользователя: учётку в панели, identity и её
// историю трафика (чтобы не повисла «не привязанной»). Основную учётку сервиса
// не трогает (identity устройства всегда длиннее: <user>-awg-<slug>).
func (m *Manager) RemoveDevice(username, device string) error {
	user, err := m.store.GetUserByName(username)
	if err != nil {
		return fmt.Errorf("пользователь не найден")
	}
	slug := util.UsernameFromDisplay(strings.TrimSpace(device))
	identity := username + "-awg-" + slug

	ids, _ := m.store.GetIdentities(user.ID)
	found := false
	for _, id := range ids {
		if id.Service == "amneziawg" && id.Identity == identity {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("устройство %q не найдено у пользователя %q", device, username)
	}

	if p, ok := m.reg.Provisioners()["amneziawg"]; ok {
		if err := p.Deprovision(identity); err != nil {
			// Учётки в панели может уже не быть — чистим БД в любом случае.
			log.Printf("[manager] remove device %s: удаление в панели: %v", identity, err)
		}
	}
	if err := m.store.DeleteIdentity("amneziawg", identity); err != nil {
		return fmt.Errorf("не удалось удалить identity: %w", err)
	}
	if err := m.store.DeleteIdentityTraffic("amneziawg", identity); err != nil {
		log.Printf("[manager] remove device %s: очистка трафика: %v", identity, err)
	}
	log.Printf("[manager] amneziawg: устройство %q пользователя %q удалено", device, username)
	return nil
}

// Regenerate пересоздаёт учётку пользователя в ОДНОМ сервисе: удаляет старую и
// создаёт новую (новые ключи/UUID/ссылки). Идентификатор в сервисе (= username)
// при этом не меняется, поэтому история трафика сохраняется, а обновляется лишь
// набор артефактов подключения (config). Возвращает свежий Account.
func (m *Manager) Regenerate(username, svc string) (*service.Account, error) {
	user, err := m.store.GetUserByName(username)
	if err != nil {
		return nil, fmt.Errorf("пользователь не найден")
	}
	p, ok := m.reg.Provisioners()[svc]
	if !ok {
		return nil, fmt.Errorf("сервис %q не поддерживает перегенерацию", svc)
	}

	// Идентификатор пользователя внутри сервиса (email/имя клиента).
	var identity string
	ids, _ := m.store.GetIdentities(user.ID)
	for _, id := range ids {
		if id.Service == svc {
			identity = id.Identity
			break
		}
	}
	if identity == "" {
		return nil, fmt.Errorf("у пользователя нет учётки в сервисе %q", svc)
	}

	// СНАЧАЛА удаляем старую учётку (панели не дают создать дубликат email),
	// и если создать новую не удалось — восстанавливаем старую, иначе у
	// пользователя не работает ничего, а в БД остаются «живые» ссылки.
	if err := p.Deprovision(identity); err != nil {
		log.Printf("[manager] regenerate %s/%s: удаление старой учётки: %v", svc, identity, err)
	}

	acc, err := p.Provision(username)
	if err != nil {
		log.Printf("[manager] regenerate %s/%s: пересоздание не удалось, откат: %v", svc, identity, err)
		if racc, rerr := p.Provision(username); rerr == nil {
			// вернули учётку (новые ключи) — сохраняем, чтобы конфиги были валидны
			if items, jerr := json.Marshal(racc.Items); jerr == nil {
				_ = m.store.AddIdentity(user.ID, racc.Service, racc.Identity, racc.Address, string(items))
			}
			return nil, fmt.Errorf("не удалось пересоздать учётку в %s (старая восстановлена, конфиги перевыпущены): %w", svc, err)
		}
		return nil, fmt.Errorf("не удалось пересоздать учётку в %s и восстановить старую — нужен /register заново: %w", svc, err)
	}

	itemsJSON, _ := json.Marshal(acc.Items)
	// identity прежний → INSERT OR REPLACE обновляет ту же строку, дельты трафика
	// (ключ — service+identity) остаются валидными.
	if err := m.store.AddIdentity(user.ID, acc.Service, acc.Identity, acc.Address, string(itemsJSON)); err != nil {
		return nil, fmt.Errorf("учётка пересоздана, но не сохранена в БД: %w", err)
	}
	log.Printf("[manager] %s: учётка пользователя %q перегенерирована", svc, username)
	return acc, nil
}

// RegenResult — результат перегенерации учётки одного пользователя.
type RegenResult struct {
	Username string `json:"username"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// RegenerateAll пересоздаёт учётки ВСЕХ пользователей в одном сервисе. Нужно,
// когда сменились серверные параметры сервиса (например, обфускация AmneziaWG)
// и все клиентские конфиги надо перевыпустить разом. Best-effort: не
// останавливается на первой ошибке, возвращает результат по каждому.
func (m *Manager) RegenerateAll(svc string) ([]RegenResult, error) {
	if _, ok := m.reg.Provisioners()[svc]; !ok {
		return nil, fmt.Errorf("сервис %q не поддерживает перегенерацию", svc)
	}
	users, err := m.store.ListUsers()
	if err != nil {
		return nil, fmt.Errorf("не удалось получить список пользователей: %w", err)
	}

	var results []RegenResult
	for _, u := range users {
		// Только те, у кого есть учётка в этом сервисе.
		ids, _ := m.store.GetIdentities(u.ID)
		has := false
		for _, id := range ids {
			if id.Service == svc {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		if _, err := m.Regenerate(u.Username, svc); err != nil {
			results = append(results, RegenResult{Username: u.Username, OK: false, Error: err.Error()})
		} else {
			results = append(results, RegenResult{Username: u.Username, OK: true})
		}
	}
	log.Printf("[manager] %s: массовая перегенерация — обработано %d учёток", svc, len(results))
	return results, nil
}

// RefreshLinks перечитывает у сервисов актуальные ссылки существующих учёток и
// сохраняет изменившиеся. Учётки не пересоздаются — в отличие от Regenerate
// клиенту ничего переимпортировать не нужно, если он подключён по подписке.
// Ошибка по одной учётке не останавливает остальные, сохранённое при этом не
// трогаем. Возвращает число обновлённых и число неудачных.
func (m *Manager) RefreshLinks() (updated, failed int) {
	for name, p := range m.reg.Provisioners() {
		lr, ok := p.(service.LinkRefresher)
		if !ok {
			continue
		}
		ids, err := m.store.IdentitiesByService(name)
		if err != nil {
			log.Printf("[manager] %s: обновление ссылок: %v", name, err)
			continue
		}
		for _, id := range ids {
			items, err := lr.RefreshItems(id.Identity)
			if err != nil {
				failed++
				log.Printf("[manager] %s/%s: ссылки не обновлены: %v", name, id.Identity, err)
				continue
			}
			itemsJSON, _ := json.Marshal(items)
			changed, err := m.store.SetIdentityConfig(name, id.Identity, string(itemsJSON))
			if err != nil {
				failed++
				log.Printf("[manager] %s/%s: ссылки не сохранены: %v", name, id.Identity, err)
				continue
			}
			if changed {
				updated++
				log.Printf("[manager] %s/%s: ссылки обновлены", name, id.Identity)
			}
		}
	}
	return updated, failed
}

// ProvisionServices возвращает имена сервисов, поддерживающих регистрацию
// (а значит и перегенерацию) — для показа доступных действий в UI.
func (m *Manager) ProvisionServices() []string { return m.reg.ProvisionNames() }

// Delete удаляет пользователя из всех сервисов и из БД.
func (m *Manager) Delete(username string) ([]ServiceResult, error) {
	user, err := m.store.GetUserByName(username)
	if err != nil {
		return nil, fmt.Errorf("пользователь не найден")
	}

	ids, _ := m.store.GetIdentities(user.ID)
	provisioners := m.reg.Provisioners()
	log.Printf("[manager] удаляю %q: идентичностей=%d", username, len(ids))

	var results []ServiceResult
	for _, id := range ids {
		p, ok := provisioners[id.Service]
		if !ok {
			log.Printf("[manager] %s/%s: сервис не поддерживает депровижининг — пропуск", id.Service, id.Identity)
			continue
		}
		if err := p.Deprovision(id.Identity); err != nil {
			log.Printf("[manager] %s/%s: ошибка удаления: %v", id.Service, id.Identity, err)
			results = append(results, ServiceResult{Service: id.Service, OK: false, Error: err.Error()})
		} else {
			log.Printf("[manager] %s/%s: удалён из сервиса", id.Service, id.Identity)
			results = append(results, ServiceResult{Service: id.Service, OK: true})
		}
	}

	if err := m.store.DeleteUser(user.ID); err != nil {
		return results, fmt.Errorf("ошибка удаления из БД: %w", err)
	}
	return results, nil
}
