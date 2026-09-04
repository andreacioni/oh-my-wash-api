package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupLatestRequest(t *testing.T, app *Application, userID string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/latest", nil)
	c.Set("userID", userID)

	app.HandleGetLatestMessage(c)

	return recorder
}

func TestHandleGetLatestMessage_DisablesCachingForSuccessResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	app := NewApplication()
	app.storage = NewMemoryStorage()

	key := "user:user-1:latest"
	if err := app.storage.Set(context.Background(), key, `{"device-1":{"deviceId":"device-1","productDate":1725440000000}}`); err != nil {
		t.Fatalf("failed to seed storage: %v", err)
	}

	recorder := setupLatestRequest(t, app, "user-1")

	if got := recorder.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, max-age=0" {
		t.Fatalf("unexpected Cache-Control header: %q", got)
	}
	if got := recorder.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("unexpected Pragma header: %q", got)
	}
	if got := recorder.Header().Get("Expires"); got != "0" {
		t.Fatalf("unexpected Expires header: %q", got)
	}
}

func TestHandleGetLatestMessage_DisablesCachingWhenNoData(t *testing.T) {
	gin.SetMode(gin.TestMode)

	app := NewApplication()
	app.storage = NewMemoryStorage()

	recorder := setupLatestRequest(t, app, "user-2")

	if got := recorder.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, max-age=0" {
		t.Fatalf("unexpected Cache-Control header: %q", got)
	}
	if got := recorder.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("unexpected Pragma header: %q", got)
	}
	if got := recorder.Header().Get("Expires"); got != "0" {
		t.Fatalf("unexpected Expires header: %q", got)
	}
}
