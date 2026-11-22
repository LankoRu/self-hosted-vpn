package nginxmap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	out, err := Render(State{
		TelemtDomain: "petrovich.ru",
		Inbounds: []Inbound{
			{Port: 10090, ServerNames: []string{"www.google.com", "google.com"}},
			{Port: 10092, ServerNames: []string{"www.microsoft.com"}},
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"$shvpn_backend", "petrovich.ru telemt;", "www.google.com xray_10090;",
		"google.com xray_10090;", "www.microsoft.com xray_10092;", "default web;",
		"upstream xray_10090", "server 127.0.0.1:10090;", "upstream xray_10092",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("карта не содержит %q:\n%s", want, out)
		}
	}
}

func TestRenderCollisions(t *testing.T) {
	cases := []State{
		{TelemtDomain: "", Inbounds: []Inbound{{Port: 1, ServerNames: []string{"a.com"}}}},
		{TelemtDomain: "www.google.com", Inbounds: []Inbound{{Port: 1, ServerNames: []string{"www.google.com"}}}},
		{TelemtDomain: "x.com", Inbounds: []Inbound{
			{Port: 1, ServerNames: []string{"a.com"}},
			{Port: 2, ServerNames: []string{"a.com"}},
		}},
		{TelemtDomain: "x.com", Inbounds: []Inbound{{Port: 1, ServerNames: nil}}},
	}
	for i, s := range cases {
		if _, err := Render(s); err == nil {
			t.Errorf("кейс %d: ожидалась ошибка", i)
		}
	}
}

func TestApplyRollbackOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "map.conf")
	if err := os.WriteFile(path, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := State{TelemtDomain: "x.com", Inbounds: []Inbound{{Port: 10090, ServerNames: []string{"a.com"}}}}
	err := Apply(path, s, func() error { return fmt.Errorf("nginx -t failed") })
	if err == nil {
		t.Fatal("ожидалась ошибка reload")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "OLD" {
		t.Errorf("откат не сработал: %q", b)
	}
	// успешное применение
	if err := Apply(path, s, func() error { return nil }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "a.com xray_10090;") {
		t.Errorf("карта не записана: %q", b)
	}
}
