package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveOrigin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		requestOrigin string
		allowed       []string
		want          string
	}{
		{
			name:          "wildcard allows any origin",
			requestOrigin: "https://evil.com",
			allowed:       []string{"*"},
			want:          "*",
		},
		{
			name:          "exact match echoes origin",
			requestOrigin: "https://app.example.com",
			allowed:       []string{"https://app.example.com"},
			want:          "https://app.example.com",
		},
		{
			name:          "no match returns empty",
			requestOrigin: "https://evil.com",
			allowed:       []string{"https://app.example.com"},
			want:          "",
		},
		{
			name:          "empty allowed list returns empty",
			requestOrigin: "https://app.example.com",
			allowed:       nil,
			want:          "",
		},
		{
			name:          "wildcard in mixed list wins",
			requestOrigin: "https://random.com",
			allowed:       []string{"https://ok.com", "*"},
			want:          "*",
		},
		{
			name:          "empty request origin no match",
			requestOrigin: "",
			allowed:       []string{"https://app.example.com"},
			want:          "",
		},
		{
			name:          "empty request origin with wildcard",
			requestOrigin: "",
			allowed:       []string{"*"},
			want:          "*",
		},
		{
			name:          "multiple origins exact match",
			requestOrigin: "https://admin.example.com",
			allowed:       []string{"https://app.example.com", "https://admin.example.com"},
			want:          "https://admin.example.com",
		},
		{
			name:          "multiple origins no match",
			requestOrigin: "https://evil.com",
			allowed:       []string{"https://app.example.com", "https://admin.example.com"},
			want:          "",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveOrigin(tt.requestOrigin, tt.allowed)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCheckWebSocketOrigin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		requestOrigin string
		allowed       []string
		want          bool
	}{
		{
			name:          "missing origin allowed against pinned list (D05: non-browser SDK)",
			requestOrigin: "",
			allowed:       []string{"http://127.0.0.1:8888", "http://localhost:8888"},
			want:          true,
		},
		{
			name:          "missing origin allowed under wildcard",
			requestOrigin: "",
			allowed:       []string{"*"},
			want:          true,
		},
		{
			name:          "matching origin allowed",
			requestOrigin: "https://app.example.com",
			allowed:       []string{"https://app.example.com"},
			want:          true,
		},
		{
			name:          "non-matching origin refused",
			requestOrigin: "https://evil.com",
			allowed:       []string{"https://app.example.com"},
			want:          false,
		},
		{
			name:          "empty allowed list refuses even a missing origin",
			requestOrigin: "",
			allowed:       nil,
			want:          false,
		},
		{
			name:          "empty allowed list refuses a present origin",
			requestOrigin: "https://app.example.com",
			allowed:       nil,
			want:          false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, CheckWebSocketOrigin(tt.requestOrigin, tt.allowed))
		})
	}
}

func TestCORSMiddleware(t *testing.T) {
	t.Parallel()

	t.Run("wildcard preserves backward compat", func(t *testing.T) {
		t.Parallel()
		mw := CORSMiddleware(func() []string { return []string{"*"} })
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		req.Header.Set("Origin", "https://any.com")
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Empty(t, rec.Header().Get("Vary"), "wildcard should not set Vary: Origin")
		require.Equal(t, "GET, POST, PUT, PATCH, DELETE, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
		require.Equal(t, "Content-Type, Authorization, X-Api-Key", rec.Header().Get("Access-Control-Allow-Headers"))
	})

	t.Run("matching origin echoes back with Vary", func(t *testing.T) {
		t.Parallel()
		mw := CORSMiddleware(func() []string {
			return []string{"https://app.example.com", "https://admin.example.com"}
		})
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		req.Header.Set("Origin", "https://admin.example.com")
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "https://admin.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, "Origin", rec.Header().Get("Vary"))
	})

	t.Run("non-matching origin gets no CORS headers", func(t *testing.T) {
		t.Parallel()
		mw := CORSMiddleware(func() []string {
			return []string{"https://app.example.com"}
		})
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		req.Header.Set("Origin", "https://evil.com")
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("OPTIONS returns 200 without calling next", func(t *testing.T) {
		t.Parallel()
		called := false
		mw := CORSMiddleware(func() []string { return []string{"*"} })
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})
		req := httptest.NewRequest(http.MethodOptions, "/api/sessions", nil)
		req.Header.Set("Origin", "https://any.com")
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.False(t, called, "OPTIONS should not call next handler")
		require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("empty origins list gets no CORS headers", func(t *testing.T) {
		t.Parallel()
		mw := CORSMiddleware(func() []string { return nil })
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		req.Header.Set("Origin", "https://any.com")
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("no Origin header gets no CORS headers for specific origins", func(t *testing.T) {
		t.Parallel()
		mw := CORSMiddleware(func() []string {
			return []string{"https://app.example.com"}
		})
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		// No Origin header set
		rec := httptest.NewRecorder()

		mw(inner).ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

func TestSetCORSHeaders(t *testing.T) {
	t.Parallel()

	t.Run("wildcard sets headers without Vary", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		SetCORSHeaders(rec, []string{"*"}, "https://any.com")

		require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Empty(t, rec.Header().Get("Vary"), "wildcard should not set Vary: Origin")
	})

	t.Run("matching origin sets headers with Vary", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		SetCORSHeaders(rec, []string{"https://app.example.com"}, "https://app.example.com")

		require.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, "Origin", rec.Header().Get("Vary"), "echo-back mode must set Vary: Origin")
	})

	t.Run("non-matching origin sets no headers", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		SetCORSHeaders(rec, []string{"https://app.example.com"}, "https://evil.com")

		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("empty origins sets no headers", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		SetCORSHeaders(rec, nil, "https://any.com")

		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}
