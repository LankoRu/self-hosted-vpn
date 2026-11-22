BINARY := vps-admin
CMD := ./cmd/vps-admin
# Версия для internal/version (видна в «Система» → «Компоненты»).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/LankoRu/vps-admin/internal/version.Version=$(VERSION)

# --- Параметры деплоя по SSH -------------------------------------------------
# Обязательно: DEPLOY_HOST=user@host   (напр. make deploy DEPLOY_HOST=root@1.2.3.4)
# Необязательно: SSH_PORT (по умолчанию 22).
DEPLOY_HOST ?=
SSH_PORT    ?= 22
BIN_DIR     ?= /opt/vps-admin
CONF_DIR    ?= /etc/vps-admin
UNIT_PATH   := /etc/systemd/system/vps-admin.service
SSH         := ssh -p $(SSH_PORT)
SCP         := scp -P $(SSH_PORT)

.PHONY: build build-linux run test tidy clean install deploy logs restart

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

build-linux:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 $(CMD)

# Локальный config.yaml с секретами не в репозитории — создаётся из примера.
config.yaml:
	cp config.example.yaml config.yaml
	@echo "Создан config.yaml из примера — впишите секреты перед запуском."

run: config.yaml
	go run $(CMD) -config ./config.yaml

test: config.yaml
	go run $(CMD) -config ./config.yaml -test

tidy:
	go mod tidy

clean:
	rm -rf bin

# Установка на сервер (запускать от root на VPS после сборки build-linux):
install:
	install -Dm755 bin/$(BINARY)-linux-amd64 /opt/vps-admin/vps-admin
	install -Dm644 config.example.yaml /etc/vps-admin/config.yaml
	install -Dm644 vps-admin.service /etc/systemd/system/vps-admin.service
	systemctl daemon-reload
	@echo "Готово. Отредактируйте /etc/vps-admin/config.yaml и: systemctl enable --now vps-admin"

# Деплой по SSH: собирает linux-бинарь и выкатывает на сервер.
# Бинарь заливается во временный файл и атомарно подменяется (mv), поэтому
# обновление работает даже при запущенном сервисе (нет ETXTBSY).
# config.yaml НЕ перезаписывается, если уже есть на сервере (чтобы не стереть секреты).
deploy: build-linux
	@test -n "$(DEPLOY_HOST)" || { echo "ОШИБКА: задайте DEPLOY_HOST, напр.: make deploy DEPLOY_HOST=root@1.2.3.4 [SSH_PORT=22]"; exit 1; }
	@echo "→ $(DEPLOY_HOST): подготовка каталогов"
	$(SSH) $(DEPLOY_HOST) 'mkdir -p $(BIN_DIR) $(CONF_DIR)'
	@echo "→ загрузка бинаря"
	$(SCP) bin/$(BINARY)-linux-amd64 $(DEPLOY_HOST):$(BIN_DIR)/vps-admin.new
	@echo "→ загрузка unit-файла"
	$(SCP) vps-admin.service $(DEPLOY_HOST):$(UNIT_PATH)
	@echo "→ загрузка config.yaml (только если его ещё нет)"
	$(SSH) $(DEPLOY_HOST) 'test -f $(CONF_DIR)/config.yaml && echo "  config.yaml уже есть — пропускаю" || (cat > $(CONF_DIR)/config.yaml; echo "  config.yaml создан — заполните секреты!")' < config.example.yaml
	@echo "→ установка и перезапуск сервиса"
	$(SSH) $(DEPLOY_HOST) 'mv -f $(BIN_DIR)/vps-admin.new $(BIN_DIR)/vps-admin && chmod +x $(BIN_DIR)/vps-admin && systemctl daemon-reload && systemctl enable vps-admin >/dev/null 2>&1; systemctl restart vps-admin; sleep 1; systemctl --no-pager --lines=0 status vps-admin || true'
	@echo "✓ Деплой завершён. Логи: make logs DEPLOY_HOST=$(DEPLOY_HOST)"

# Перезапуск сервиса на сервере без пересборки.
restart:
	@test -n "$(DEPLOY_HOST)" || { echo "ОШИБКА: задайте DEPLOY_HOST"; exit 1; }
	$(SSH) $(DEPLOY_HOST) 'systemctl restart vps-admin && systemctl --no-pager --lines=0 status vps-admin'

# Живой просмотр логов сервиса на сервере.
logs:
	@test -n "$(DEPLOY_HOST)" || { echo "ОШИБКА: задайте DEPLOY_HOST"; exit 1; }
	$(SSH) $(DEPLOY_HOST) 'journalctl -u vps-admin -f -n 100'
