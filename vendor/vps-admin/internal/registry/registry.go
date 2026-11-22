// Package registry собирает включённые сервисы и предоставляет доступ к их
// коллекторам и провижинерам.
package registry

import (
	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/services/awg3xui"
	"github.com/LankoRu/vps-admin/internal/services/telemt"
	"github.com/LankoRu/vps-admin/internal/services/xray"
	"github.com/LankoRu/vps-admin/internal/store"
)

// Registry — набор активных сервисов.
type Registry struct {
	collectors   []service.Collector
	provisioners map[string]service.Provisioner // service name -> provisioner
}

// New строит реестр из конфига.
func New(cfg *config.Config, st *store.Store) *Registry {
	r := &Registry{provisioners: make(map[string]service.Provisioner)}

	if cfg.Services.Xray.Enabled {
		svc := xray.New(cfg)
		r.collectors = append(r.collectors, svc)
		if cfg.Services.Xray.CanProvision() {
			r.provisioners[svc.Name()] = svc
		}
	}
	if cfg.Services.AmneziaWG.Enabled {
		// AmneziaWG — через инбаунд панели 3x-ui (единственный бэкенд).
		svc := awg3xui.New(cfg)
		r.collectors = append(r.collectors, svc)
		if cfg.Services.AmneziaWG.CanProvision() {
			r.provisioners[svc.Name()] = svc
		}
	}
	if cfg.Services.TeleMT.Enabled {
		svc := telemt.New(cfg)
		r.collectors = append(r.collectors, svc)
		if cfg.Services.TeleMT.CanProvision() {
			r.provisioners[svc.Name()] = svc
		}
	}
	return r
}

// NewWithProvisioners строит реестр из готового набора провижинеров (без
// коллекторов). Используется в тестах, где сервисы подменяются заглушками.
func NewWithProvisioners(provisioners map[string]service.Provisioner) *Registry {
	if provisioners == nil {
		provisioners = make(map[string]service.Provisioner)
	}
	return &Registry{provisioners: provisioners}
}

// Collectors возвращает все активные коллекторы.
func (r *Registry) Collectors() []service.Collector { return r.collectors }

// Provisioners возвращает провижинеры с их именами.
func (r *Registry) Provisioners() map[string]service.Provisioner { return r.provisioners }

// ProvisionNames возвращает отсортированный список имён сервисов, поддерживающих
// регистрацию (для стабильного порядка вывода не важно — их три).
func (r *Registry) ProvisionNames() []string {
	names := make([]string, 0, len(r.provisioners))
	for name := range r.provisioners {
		names = append(names, name)
	}
	return names
}
