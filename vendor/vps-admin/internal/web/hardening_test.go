package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQRRejectsOversizedText(t *testing.T) {
	large := strings.Repeat("a", maxQRText+1)
	req := httptest.NewRequest("GET", "/api/qr?text="+large, nil)
	rec := httptest.NewRecorder()
	// публичный эндпоинт: вызываем хендлер напрямую, без сервера
	srv := &Server{}
	srv.handleQR(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestQRAcceptsNormalText(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/qr?text=vless://short", nil)
	rec := httptest.NewRecorder()
	(&Server{}).handleQR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestIsHTTPSBehindProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/login", nil) // r.TLS == nil (за nginx)
	if isHTTPS(r) {
		t.Error("без заголовков не должен считаться HTTPS")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !isHTTPS(r) {
		t.Error("за nginx (X-Forwarded-Proto: https) должен считаться HTTPS — иначе cookie без Secure")
	}
	r2 := httptest.NewRequest("POST", "/api/login", nil)
	r2.Header.Set("X-Forwarded-Proto", "http")
	if isHTTPS(r2) {
		t.Error("http не должен считаться HTTPS")
	}
}
