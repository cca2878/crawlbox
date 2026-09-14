package web

import (
	"github.com/cca2878/crawlbox/internal/model"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/app"
	"github.com/cca2878/crawlbox/internal/catalog"
	"golang.org/x/crypto/bcrypt"
)

func TestUIPagesAndTokenFlow(t *testing.T) {
	store, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Register(t.Context(), map[string]string{"alpha": "fixture"}); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{App: &app.App{Store: store}, Credentials: Credentials{Username: "admin", PasswordHash: string(hash)}}
	handler := server.Handler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.SetBasicAuth("admin", "password")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for page, heading := range map[string]string{"sources": "数据源", "runs": "运行记录", "revisions": "版本历史", "tokens": "API Token", "settings": "服务设置", "unknown": "数据源"} {
		w := request("GET", "/ui/?page="+page, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<h2>"+heading+"</h2>") || strings.Count(w.Body.String(), "<h2>") != 1 {
			t.Fatalf("page %s: %d %s", page, w.Code, w.Body.String())
		}
	}
	w := request("POST", "/ui/tokens", url.Values{"name": {"<script>test</script>"}, "sources": {"alpha"}}.Encode())
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "请保存 Token") || strings.Contains(w.Body.String(), "<script>test</script>") || !strings.HasSuffix(strings.TrimSpace(w.Body.String()), "</html>") {
		t.Fatalf("token response: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(request("GET", "/ui/?page=tokens", "").Body.String(), "请保存 Token") {
		t.Fatal("secret reappeared on GET")
	}
	tokens, err := store.Tokens(t.Context())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens: %v %v", tokens, err)
	}
	w = request("POST", "/ui/tokens/"+tokens[0].ID+"/revoke", "")
	if w.Code != 303 || w.Header().Get("Location") != "/ui/?page=tokens" {
		t.Fatalf("revoke: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestRunResultReplacesStaleProgress(t *testing.T) {
	store, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{App: &app.App{Store: store}}
	for status, want := range map[string]string{
		"running":     "正在下载测试文件",
		"succeeded":   "采集成功，已发布新版本。",
		"no_change":   "检查完成，无变化。",
		"failed":      "任务失败，请查看错误详情。",
		"interrupted": "任务已中断。",
	} {
		t.Run(status, func(t *testing.T) {
			run := model.Run{ID: "test-run", Source: "alpha", Status: status, Started: time.Now(), Progress: "正在下载测试文件"}
			if status != "running" {
				finished := time.Now()
				run.Finished = &finished
			}
			if status == "failed" {
				run.Error = "upstream unavailable"
			}
			if err := store.SaveRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			server.render(w, httptest.NewRequest("GET", "/ui/?page=runs", nil), "")
			body := w.Body.String()
			if !strings.Contains(body, want) {
				t.Fatalf("missing result %q", want)
			}
			if status != "running" && strings.Contains(body, run.Progress) {
				t.Fatal("stale progress shown as result")
			}
			if run.Error != "" && !strings.Contains(body, run.Error) {
				t.Fatal("error details missing")
			}
		})
	}
}

func TestRunMessageEscapingAndOutcomeBoundary(t *testing.T) {
	store, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{App: &app.App{Store: store}}
	for _, status := range []string{"succeeded", "no_change", "running", "failed", "interrupted"} {
		run := model.Run{ID: "message", Source: "alpha", Status: status, Started: time.Now(), Message: "<script>custom-result</script>"}
		if err := store.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		server.render(w, httptest.NewRequest("GET", "/ui/?page=runs", nil), "")
		body := w.Body.String()
		if strings.Contains(body, "<script>") {
			t.Fatal("result interpreted as HTML")
		}
		want := status == "succeeded" || status == "no_change"
		if strings.Contains(body, "&lt;script&gt;custom-result&lt;/script&gt;") != want {
			t.Fatalf("message exposure for %s", status)
		}
	}
}
