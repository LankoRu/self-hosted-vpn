// Package version — версия бинаря vps-admin. Пробрасывается при сборке:
// CI: -ldflags "-X github.com/LankoRu/vps-admin/internal/version.Version=v0.1.x"
// Makefile: git describe. Без ldflags — "dev".
package version

var Version = "dev"
