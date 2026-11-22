// Service worker нужен, чтобы браузер предлагал установку приложения.
// Кэшируем только оболочку и иконки; API и страницы с ключами НЕ кэшируем —
// панель должна показывать актуальные данные, а конфиги не должны оседать
// в кэше устройства.
const SHELL = "vps-admin-shell-v1";
const ASSETS = ["/", "/icon-192.png", "/icon-512.png"];

self.addEventListener("install", e => {
  e.waitUntil(caches.open(SHELL).then(c => c.addAll(ASSETS)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== SHELL).map(k => caches.delete(k))))
      .then(() => self.clients.claim()));
});

self.addEventListener("fetch", e => {
  const url = new URL(e.request.url);
  // Всё динамическое — только из сети.
  if (e.request.method !== "GET" || url.pathname.startsWith("/api/") || url.pathname.startsWith("/p/")) return;
  // Оболочка: сеть, а при офлайне — кэш (чтобы приложение открылось).
  e.respondWith(
    fetch(e.request)
      .then(r => {
        if (r.ok && ASSETS.includes(url.pathname)) {
          const copy = r.clone();
          caches.open(SHELL).then(c => c.put(e.request, copy));
        }
        return r;
      })
      .catch(() => caches.match(e.request).then(r => r || caches.match("/"))));
});
