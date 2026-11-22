package xray

// connections.go — управление параметрами подключения из панели vps-admin
// («Подключения»): ротация Reality-SNI/dest/fingerprint, запасной транспорт
// (XHTTP/gRPC), attach/detach клиентов. Всё через API 3x-ui; nginx SNI-карту
// перегенерирует manager (пакет nginxmap).

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/util"
)

// BackupRemarkPrefix — remark запасных инбаундов; по нему находим их в панели.
const BackupRemarkPrefix = "shvpn-backup-"

// listInboundsFull — инбаунды панели «сырыми» объектами (все поля нужны для
// update: панель принимает инбаунд целиком).
func (s *Service) listInboundsFull() ([]map[string]json.RawMessage, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/inbounds/list"))
	if err != nil {
		return nil, transportHint(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("inbounds/list", resp.StatusCode, body)
	}
	var r struct {
		Obj []map[string]json.RawMessage `json:"obj"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode inbounds: %w", err)
	}
	return r.Obj, nil
}

func (s *Service) inboundByID(id int) (map[string]json.RawMessage, error) {
	list, err := s.listInboundsFull()
	if err != nil {
		return nil, err
	}
	for _, ib := range list {
		var ibID int
		_ = json.Unmarshal(ib["id"], &ibID)
		if ibID == id {
			return ib, nil
		}
	}
	return nil, fmt.Errorf("инбаунд %d не найден в панели", id)
}

// streamReality — разбирает streamSettings инбаунда в map (для правки на месте).
func streamReality(raw json.RawMessage) (map[string]interface{}, map[string]interface{}, error) {
	var stream map[string]interface{}
	if err := json.Unmarshal(jsonBytes(raw), &stream); err != nil {
		return nil, nil, fmt.Errorf("streamSettings: %w", err)
	}
	sec, _ := stream["security"].(string)
	if sec != "reality" {
		return nil, nil, fmt.Errorf("инбаунд не Reality (security=%q)", sec)
	}
	rs, _ := stream["realitySettings"].(map[string]interface{})
	if rs == nil {
		return nil, nil, fmt.Errorf("в streamSettings нет realitySettings")
	}
	return stream, rs, nil
}

// Reality читает Reality-параметры инбаунда. Поддерживает оба формата:
// старый (dest/fingerprint/publicKey на верхнем уровне) и новый Xray 25.x /
// 3x-ui 3.11 (target + вложенный settings{publicKey,fingerprint,spiderX}).
func (s *Service) Reality(inboundID int) (*service.RealitySettings, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}
	ib, err := s.inboundByID(inboundID)
	if err != nil {
		return nil, err
	}
	_, rs, err := streamReality(ib["streamSettings"])
	if err != nil {
		return nil, err
	}
	out := &service.RealitySettings{}
	out.Dest, _ = rs["dest"].(string)
	if out.Dest == "" {
		out.Dest, _ = rs["target"].(string)
	}
	out.Fingerprint, _ = rs["fingerprint"].(string)
	out.PublicKey, _ = rs["publicKey"].(string)
	if nested, ok := rs["settings"].(map[string]interface{}); ok {
		if out.Fingerprint == "" {
			out.Fingerprint, _ = nested["fingerprint"].(string)
		}
		if out.PublicKey == "" {
			out.PublicKey, _ = nested["publicKey"].(string)
		}
	}
	if names, ok := rs["serverNames"].([]interface{}); ok {
		for _, n := range names {
			if str, ok := n.(string); ok {
				out.ServerNames = append(out.ServerNames, str)
			}
		}
	}
	if ids, ok := rs["shortIds"].([]interface{}); ok {
		for _, n := range ids {
			if str, ok := n.(string); ok {
				out.ShortIDs = append(out.ShortIDs, str)
			}
		}
	}
	return out, nil
}

// updateInbound отправляет инбаунд целиком в /panel/api/inbounds/update/:id.
// settings/streamSettings нормализуются в JSON-строки (панель хранит их так).
// ВНИМАНИЕ: панель перезапускает xray-core — кратковременный разрыв соединений.
func (s *Service) updateInbound(ib map[string]json.RawMessage, stream map[string]interface{}) error {
	var id int
	if err := json.Unmarshal(ib["id"], &id); err != nil {
		return fmt.Errorf("id инбаунда: %w", err)
	}
	payload := map[string]interface{}{}
	for k, v := range ib {
		var any interface{}
		if err := json.Unmarshal(v, &any); err == nil {
			payload[k] = any
		}
	}
	payload["settings"] = string(jsonBytes(ib["settings"]))
	if stream != nil {
		sb, _ := json.Marshal(stream)
		payload["streamSettings"] = string(sb)
	} else {
		payload["streamSettings"] = string(jsonBytes(ib["streamSettings"]))
	}
	body, _ := json.Marshal(payload)
	_, err := s.postAPI(fmt.Sprintf("/panel/api/inbounds/update/%d", id), body)
	return err
}

// UpdateReality правит Reality-параметры инбаунда. Непустые поля rs заменяют
// текущие; ключи (publicKey/privateKey) и shortIds не трогаются.
func (s *Service) UpdateReality(inboundID int, rs *service.RealitySettings) error {
	if err := s.auth(); err != nil {
		return fmt.Errorf("xray: %w", err)
	}
	ib, err := s.inboundByID(inboundID)
	if err != nil {
		return err
	}
	stream, reality, err := streamReality(ib["streamSettings"])
	if err != nil {
		return err
	}
	if rs.Dest != "" {
		// новый формат (Xray 25.x): target + settings.fingerprint;
		// старый: dest + fingerprint на верхнем уровне.
		if _, isNew := reality["target"]; isNew {
			reality["target"] = withPort(rs.Dest)
			nested, _ := reality["settings"].(map[string]interface{})
			if nested == nil {
				nested = map[string]interface{}{}
			}
			if rs.Fingerprint != "" {
				nested["fingerprint"] = rs.Fingerprint
			}
			reality["settings"] = nested
		} else {
			reality["dest"] = withPort(rs.Dest)
			if rs.Fingerprint != "" {
				reality["fingerprint"] = rs.Fingerprint
			}
		}
	}
	if len(rs.ServerNames) > 0 {
		reality["serverNames"] = rs.ServerNames
	}
	if rs.Fingerprint != "" {
		if _, isNew := reality["target"]; !isNew {
			reality["fingerprint"] = rs.Fingerprint
		}
	}
	stream["realitySettings"] = reality
	return s.updateInbound(ib, stream)
}

// withPort — dest/target в панели хранится как host:port.
func withPort(dest string) string {
	if strings.Contains(dest, ":") {
		return dest
	}
	return dest + ":443"
}

// SetEnabled включает/выключает инбаунд (kill-switch сервиса/транспорта).
// В 3x-ui для этого ОТДЕЛЬНЫЙ эндпоинт setEnable — /update поле enable игнорирует.
func (s *Service) SetEnabled(inboundID int, enabled bool) error {
	if err := s.auth(); err != nil {
		return fmt.Errorf("xray: %w", err)
	}
	body, _ := json.Marshal(map[string]interface{}{"enable": enabled})
	_, err := s.postAPI(fmt.Sprintf("/panel/api/inbounds/setEnable/%d", inboundID), body)
	return err
}

// XrayInbounds — включённые Reality-инбаунды vless для nginx SNI-карты.
func (s *Service) XrayInbounds() ([]service.InboundNet, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}
	list, err := s.listInboundsFull()
	if err != nil {
		return nil, err
	}
	var out []service.InboundNet
	for _, ib := range list {
		var proto, remark string
		var id, port int
		var enabled bool
		_ = json.Unmarshal(ib["protocol"], &proto)
		_ = json.Unmarshal(ib["remark"], &remark)
		_ = json.Unmarshal(ib["id"], &id)
		_ = json.Unmarshal(ib["port"], &port)
		_ = json.Unmarshal(ib["enable"], &enabled)
		if proto != "vless" && proto != "vmess" && proto != "trojan" {
			continue
		}
		net := service.InboundNet{ID: id, Port: port, Protocol: proto, Remark: remark, Enabled: enabled}
		if _, rs, err := streamReality(ib["streamSettings"]); err == nil {
			if names, ok := rs["serverNames"].([]interface{}); ok {
				for _, n := range names {
					if str, ok := n.(string); ok {
						net.ServerNames = append(net.ServerNames, str)
					}
				}
			}
		}
		out = append(out, net)
	}
	return out, nil
}

// Attach прикрепляет существующую учётку к инбаунду (запасной транспорт).
// Трафик остаётся одной строкой на email — коллектор читает основной инбаунд,
// задвоения нет (в отличие от двух РАЗНЫХ сервисов на одной учётке).
func (s *Service) Attach(email string, inboundID int) error {
	return s.clientInboundOp(email, inboundID, "attach")
}

// Detach открепляет учётку от инбаунда.
func (s *Service) Detach(email string, inboundID int) error {
	return s.clientInboundOp(email, inboundID, "detach")
}

func (s *Service) clientInboundOp(email string, inboundID int, op string) error {
	if err := s.auth(); err != nil {
		return fmt.Errorf("xray: %w", err)
	}
	body, _ := json.Marshal(map[string]interface{}{"inboundIds": []int{inboundID}})
	_, err := s.postAPI("/panel/api/clients/"+url.PathEscape(email)+"/"+op, body)
	return err
}

// FindBackup ищет запасной инбаунд по remark shvpn-backup-<transport>.
// Возвращает (0, false, nil), если не найден.
func (s *Service) FindBackup(transport string) (int, bool, error) {
	list, err := s.listInboundsFull()
	if err != nil {
		return 0, false, err
	}
	for _, ib := range list {
		var remark string
		var id int
		var enabled bool
		_ = json.Unmarshal(ib["remark"], &remark)
		_ = json.Unmarshal(ib["id"], &id)
		_ = json.Unmarshal(ib["enable"], &enabled)
		if remark == BackupRemarkPrefix+transport {
			return id, enabled, nil
		}
	}
	return 0, false, nil
}

// EnsureBackup создаёт (или находит) запасной инбаунд vless+Reality заданного
// транспорта на localhost-порту. Клиенты подключаются отдельно (Attach).
func (s *Service) EnsureBackup(transport string, port int, rs *service.RealitySettings) (int, error) {
	if err := s.auth(); err != nil {
		return 0, fmt.Errorf("xray: %w", err)
	}
	if transport != "xhttp" && transport != "grpc" {
		return 0, fmt.Errorf("неподдерживаемый запасной транспорт: %q (xhttp|grpc)", transport)
	}
	if id, _, err := s.FindBackup(transport); err != nil {
		return 0, err
	} else if id != 0 {
		// Уже есть — обновляем Reality-параметры и включаем.
		if err := s.UpdateReality(id, rs); err != nil {
			return 0, err
		}
		if err := s.SetEnabled(id, true); err != nil {
			return 0, err
		}
		return id, nil
	}

	priv, pub, err := generateRealityKeys()
	if err != nil {
		return 0, err
	}
	shortID, err := util.RandHex(4)
	if err != nil {
		return 0, err
	}
	fp := rs.Fingerprint
	if fp == "" {
		fp = "chrome"
	}
	stream := map[string]interface{}{
		"network":  transport,
		"security": "reality",
		"realitySettings": map[string]interface{}{
			"show":        false,
			"xver":        0,
			"dest":        withPort(rs.Dest), // старый формат (для совместимости)
			"target":      withPort(rs.Dest), // новый формат Xray 25.x / 3x-ui 3.11
			"serverNames": rs.ServerNames,
			"privateKey":  priv,
			"shortIds":    []string{shortID},
			"fingerprint": fp,
			"settings": map[string]interface{}{
				"publicKey":   pub,
				"fingerprint": fp,
				"serverName":  "",
				"spiderX":     "/",
			},
		},
	}
	switch transport {
	case "xhttp":
		stream["xhttpSettings"] = map[string]interface{}{"path": "/shvpn", "mode": "auto"}
	case "grpc":
		stream["grpcSettings"] = map[string]interface{}{"serviceName": "shvpn", "multiMode": true}
	}
	settings, _ := json.Marshal(map[string]interface{}{
		"clients": []interface{}{}, "decryption": "none", "fallbacks": []interface{}{},
	})
	streamB, _ := json.Marshal(stream)
	payload, _ := json.Marshal(map[string]interface{}{
		"up": 0, "down": 0, "total": 0,
		"remark": BackupRemarkPrefix + transport, "enable": true, "expiryTime": 0,
		"listen": "127.0.0.1", "port": port, "protocol": "vless",
		"settings": string(settings), "streamSettings": string(streamB),
		"sniffing": map[string]interface{}{"enabled": true, "destOverride": []string{"http", "tls", "quic"}},
	})
	if _, err := s.postAPI("/panel/api/inbounds/add", payload); err != nil {
		return 0, fmt.Errorf("создание запасного инбаунда: %w", err)
	}
	id, _, err := s.FindBackup(transport)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("запасной инбаунд создан, но не найден в списке: %v", err)
	}
	return id, nil
}

// generateRealityKeys — пара X25519 для Reality (base64 RawURL, как у xray).
func generateRealityKeys() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(k.Bytes()),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// BackupServerNames — serverNames запасного инбаунда (для состояния/карты).
func (s *Service) BackupServerNames(transport string) ([]string, string, error) {
	id, _, err := s.FindBackup(transport)
	if err != nil || id == 0 {
		return nil, "", err
	}
	rs, err := s.Reality(id)
	if err != nil {
		return nil, "", err
	}
	return rs.ServerNames, rs.Dest, nil
}

// MainInboundID — основной инбаунд из конфига (для manager).
func (s *Service) MainInboundID() int { return s.cfg.InboundID }

// BackupPort — localhost-порт запасного инбаунда из конфига.
func (s *Service) BackupPort() int { return s.cfg.BackupPort }
