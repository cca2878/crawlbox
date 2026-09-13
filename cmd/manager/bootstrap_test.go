package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.org/crawler/manager/internal/config"
	"example.org/crawler/manager/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func TestFirstAdministrator(t *testing.T) {
	dir := t.TempDir()
	completed := 0
	handler := setupHandler(dir, func() { completed++ })
	post := func(password, origin string) *httptest.ResponseRecorder {
		form := url.Values{"username": {"admin"}, "password": {password}, "confirm": {password}}
		r := httptest.NewRequest("POST", "http://localhost/ui/setup", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := post("a-long-test-password", "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	if w := post("short", ""); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.yaml")); !os.IsNotExist(err) {
		t.Fatal("invalid request persisted credentials")
	}
	password := "test-password-$-with:quotes"
	if w := post(password, ""); w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code)
	}
	if w := post("a-different-password", ""); w.Code != http.StatusConflict {
		t.Fatal(w.Code)
	}
	if completed != 1 {
		t.Fatal(completed)
	}
	path, err := prepareConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := web.LoadCredentials(cfg.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err = bcrypt.CompareHashAndPassword([]byte(credentials.PasswordHash), []byte(password)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err = ensureAdministrator(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareConfig(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("configuration overwritten")
	}
	info, _ := os.Stat(filepath.Join(dir, "admin.yaml"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("credentials permissions")
	}
}

func TestResetPassword(t *testing.T) {
	dir := t.TempDir()
	handler := setupHandler(dir, func() {})
	form := url.Values{"username": {"owner"}, "password": {"original-password"}, "confirm": {"original-password"}}
	r := httptest.NewRequest("POST", "http://localhost/ui/setup", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	path := filepath.Join(dir, "admin.yaml")
	if err := resetPassword(path, strings.NewReader("replacement-password\n")); err != nil {
		t.Fatal(err)
	}
	c, err := web.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "owner" {
		t.Fatal("username changed")
	}
	if bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte("replacement-password")) != nil {
		t.Fatal("new password rejected")
	}
	if bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte("original-password")) == nil {
		t.Fatal("old password accepted")
	}
	if err = resetPassword(path, strings.NewReader("short")); err == nil {
		t.Fatal("short password accepted")
	}
	if err = resetPassword(filepath.Join(dir, "missing.yaml"), strings.NewReader("replacement-password")); err == nil {
		t.Fatal("missing account must not be created")
	}
}
