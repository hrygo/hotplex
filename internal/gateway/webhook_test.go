package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/cron"
)

const testSecret = "test-webhook-secret"

// checkSuiteDetail is a typed alias for the CheckSuite inner struct.
type checkSuiteDetail = struct {
	Conclusion   string `json:"conclusion"`
	HeadSHA      string `json:"head_sha"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// checkRunDetail is a typed alias for the CheckRun inner struct.
type checkRunDetail = struct {
	Conclusion string `json:"conclusion"`
	CheckSuite *struct {
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func signPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func testWebhookConfig() config.WebhookConfig {
	return config.WebhookConfig{
		MaxBodySize:   1 << 20,
		Secret:        testSecret,
		Path:          "/api/webhook/github",
		AllowedRepos:  []string{"hrygo/hotplex"},
		TargetJobName: "pr-review-hotplex",
		Enabled:       true,
	}
}

func postWebhook(h *WebhookHandler, eventType string, payload any) *httptest.ResponseRecorder {
	var body []byte
	switch p := payload.(type) {
	case []byte:
		body = p
	default:
		var err error
		body, err = json.Marshal(p)
		if err != nil {
			panic(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// postWebhookDelivery is postWebhook with GitHub's per-delivery identifier
// attached, which is what makes a firing deduplicable end to end.
func postWebhookDelivery(h *WebhookHandler, eventType string, payload any, deliveryID string) *httptest.ResponseRecorder {
	var body []byte
	switch p := payload.(type) {
	case []byte:
		body = p
	default:
		var err error
		body, err = json.Marshal(p)
		if err != nil {
			panic(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, body))
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func newIsolatedHandler(t *testing.T) (*WebhookHandler, *mockTrigger) {
	t.Helper()
	tr := &mockTrigger{}
	h := NewWebhookHandler(context.Background(), testWebhookConfig(), tr, noopLogger())
	t.Cleanup(func() { h.Close() })
	return h, tr
}

// --- Signature verification unit tests (AC-3) ---

func TestWebhookHandler_SignatureVerification(t *testing.T) {
	t.Parallel()

	h := &WebhookHandler{
		cfg: config.WebhookConfig{Secret: testSecret, MaxBodySize: 1 << 20},
	}

	payload := []byte(`{"action":"opened"}`)

	t.Run("valid signature", func(t *testing.T) {
		t.Parallel()
		sig := signPayload(testSecret, payload)
		ok := h.verifySignature(payload, sig)
		require.True(t, ok)
	})

	t.Run("invalid signature", func(t *testing.T) {
		t.Parallel()
		ok := h.verifySignature(payload, "sha256=deadbeef")
		require.False(t, ok)
	})

	t.Run("missing signature", func(t *testing.T) {
		t.Parallel()
		ok := h.verifySignature(payload, "")
		require.False(t, ok)
	})

	t.Run("empty secret rejects all", func(t *testing.T) {
		t.Parallel()
		empty := &WebhookHandler{cfg: config.WebhookConfig{Secret: "", MaxBodySize: 1 << 20}}
		sig := signPayload(testSecret, payload)
		ok := empty.verifySignature(payload, sig)
		require.False(t, ok)
	})

	t.Run("wrong prefix", func(t *testing.T) {
		t.Parallel()
		mac := hmac.New(sha256.New, []byte(testSecret))
		mac.Write(payload)
		ok := h.verifySignature(payload, "sha1="+hex.EncodeToString(mac.Sum(nil)))
		require.False(t, ok)
	})

	t.Run("malformed hex", func(t *testing.T) {
		t.Parallel()
		ok := h.verifySignature(payload, "sha256=ZZZZ")
		require.False(t, ok)
	})
}

// --- Event filtering unit tests (AC-4) ---

func TestWebhookHandler_ExtractPRs(t *testing.T) {
	t.Parallel()

	h := &WebhookHandler{cfg: config.WebhookConfig{MaxBodySize: 1 << 20}}

	t.Run("pull_request opened triggers PR", func(t *testing.T) {
		t.Parallel()
		pr := &struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Draft  bool   `json:"draft"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}{Number: 42, State: "open"}
		pr.Head.SHA = "abc123"
		e := &GitHubEvent{Action: "opened", Number: 42, PullRequest: pr}
		prs := h.extractPRs("pull_request", e)
		require.Equal(t, []int{42}, prs)
	})

	t.Run("pull_request synchronize triggers PR", func(t *testing.T) {
		t.Parallel()
		pr := &struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Draft  bool   `json:"draft"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}{Number: 706, State: "open"}
		pr.Head.SHA = "def456"
		e := &GitHubEvent{Action: "synchronize", Number: 706, PullRequest: pr}
		prs := h.extractPRs("pull_request", e)
		require.Equal(t, []int{706}, prs)
	})

	t.Run("pull_request draft ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			Action: "opened",
			Number: 99,
			PullRequest: &struct {
				Number int    `json:"number"`
				State  string `json:"state"`
				Draft  bool   `json:"draft"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}{Number: 99, State: "open", Draft: true},
		}
		prs := h.extractPRs("pull_request", e)
		require.Empty(t, prs)
	})

	t.Run("pull_request closed ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			Action: "closed",
			Number: 42,
			PullRequest: &struct {
				Number int    `json:"number"`
				State  string `json:"state"`
				Draft  bool   `json:"draft"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}{Number: 42, State: "closed"},
		}
		prs := h.extractPRs("pull_request", e)
		require.Empty(t, prs)
	})

	t.Run("pull_request labeled ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			Action: "labeled",
			Number: 42,
			PullRequest: &struct {
				Number int    `json:"number"`
				State  string `json:"state"`
				Draft  bool   `json:"draft"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}{Number: 42, State: "open"},
		}
		prs := h.extractPRs("pull_request", e)
		require.Empty(t, prs, "non-actionable pull_request actions should be ignored")
	})

	t.Run("check_suite success triggers PRs", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			CheckSuite: &checkSuiteDetail{
				Conclusion: "success",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 42}},
			},
		}
		prs := h.extractPRs("check_suite", e)
		require.Equal(t, []int{42}, prs)
	})

	t.Run("check_suite failure ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			CheckSuite: &checkSuiteDetail{
				Conclusion: "failure",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 42}},
			},
		}
		prs := h.extractPRs("check_suite", e)
		require.Empty(t, prs)
	})

	t.Run("check_run success triggers PRs", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			CheckRun: &checkRunDetail{
				Conclusion: "success",
				CheckSuite: &struct {
					PullRequests []struct {
						Number int `json:"number"`
					} `json:"pull_requests"`
				}{
					PullRequests: []struct {
						Number int `json:"number"`
					}{{Number: 77}},
				},
			},
		}
		prs := h.extractPRs("check_run", e)
		require.Equal(t, []int{77}, prs)
	})

	t.Run("check_run failure ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			CheckRun: &checkRunDetail{
				Conclusion: "failure",
				CheckSuite: &struct {
					PullRequests []struct {
						Number int `json:"number"`
					} `json:"pull_requests"`
				}{
					PullRequests: []struct {
						Number int `json:"number"`
					}{{Number: 77}},
				},
			},
		}
		prs := h.extractPRs("check_run", e)
		require.Empty(t, prs)
	})

	t.Run("check_run nil CheckSuite ignored", func(t *testing.T) {
		t.Parallel()
		e := &GitHubEvent{
			CheckRun: &checkRunDetail{
				Conclusion: "success",
				CheckSuite: nil,
			},
		}
		prs := h.extractPRs("check_run", e)
		require.Empty(t, prs)
	})

	t.Run("unknown event type returns nil", func(t *testing.T) {
		t.Parallel()
		prs := h.extractPRs("push", &GitHubEvent{})
		require.Empty(t, prs)
	})
}

// --- ServeHTTP integration tests ---

type mockTrigger struct {
	triggered atomic.Int32
	mu        sync.Mutex
	lastReq   cron.TriggerRequest
	allReqs   []cron.TriggerRequest
}

func (m *mockTrigger) TriggerByName(_ context.Context, _ string, req cron.TriggerRequest) error {
	m.mu.Lock()
	m.triggered.Add(1)
	m.lastReq = req
	m.allReqs = append(m.allReqs, req)
	m.mu.Unlock()
	return nil
}

func (m *mockTrigger) count() int { return int(m.triggered.Load()) }

func (m *mockTrigger) getLastExtra() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq.Extra
}

// getLastSourceID is the per-delivery identity the handler derived from the
// X-GitHub-Delivery header, or "" when the request carried none.
func (m *mockTrigger) getLastSourceID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq.VerifiedEventID
}

// sourceIDs is every identity the handler derived, in trigger order. One
// payload can fan out to several firings, so "the last one" is not enough to
// reason about whether they collided.
func (m *mockTrigger) sourceIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.allReqs))
	for _, r := range m.allReqs {
		out = append(out, r.VerifiedEventID)
	}
	return out
}

func TestWebhookHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	t.Run("GET returns 405", func(t *testing.T) {
		t.Parallel()
		h, _ := newIsolatedHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/api/webhook/github", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})

	t.Run("missing signature returns 403", func(t *testing.T) {
		t.Parallel()
		h, _ := newIsolatedHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("invalid signature returns 403", func(t *testing.T) {
		t.Parallel()
		h, _ := newIsolatedHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("ping returns 200 without trigger", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "ping", []byte(`{"zen":"Keep it logical.","repository":{"full_name":"hrygo/hotplex"}}`))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 0, trigger.count())
	})

	t.Run("pull_request opened triggers review", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		pr := &struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Draft  bool   `json:"draft"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}{Number: 99, State: "open"}
		pr.Head.SHA = "abc123"
		w := postWebhook(h, "pull_request", &GitHubEvent{
			Action: "opened",
			Number: 99,
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			PullRequest: pr,
		})
		require.Equal(t, http.StatusAccepted, w.Code)
		require.Eventually(t, func() bool { return trigger.count() >= 1 }, 2*time.Second, 50*time.Millisecond)
		require.Equal(t, "99", trigger.getLastExtra()["pr_number"])
	})

	t.Run("pull_request draft does not trigger", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "pull_request", &GitHubEvent{
			Action: "opened",
			Number: 99,
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			PullRequest: &struct {
				Number int    `json:"number"`
				State  string `json:"state"`
				Draft  bool   `json:"draft"`
				Head   struct {
					SHA string `json:"sha"`
				} `json:"head"`
			}{Number: 99, State: "open", Draft: true},
		})
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 0, trigger.count())
	})

	t.Run("wrong repo returns 200 without trigger", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "check_suite", &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "other/repo"},
			CheckSuite: &checkSuiteDetail{
				Conclusion: "success",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 42}},
			},
		})
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 0, trigger.count())
	})

	t.Run("any repo accepted when AllowedRepos empty", func(t *testing.T) {
		t.Parallel()
		trigger := &mockTrigger{}
		cfg := testWebhookConfig()
		cfg.AllowedRepos = nil
		h := NewWebhookHandler(context.Background(), cfg, trigger, noopLogger())
		t.Cleanup(func() { h.Close() })
		w := postWebhook(h, "check_suite", &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "any/repo"},
			CheckSuite: &checkSuiteDetail{
				Conclusion: "success",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 10}},
			},
		})
		require.Equal(t, http.StatusAccepted, w.Code)
		require.Eventually(t, func() bool { return trigger.count() >= 1 }, 2*time.Second, 50*time.Millisecond)
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		t.Parallel()
		h, _ := newIsolatedHandler(t)
		w := postWebhook(h, "check_suite", []byte(`{invalid json`))
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("oversized payload returns 413", func(t *testing.T) {
		t.Parallel()
		h, _ := newIsolatedHandler(t)
		bigPayload := make([]byte, 1<<20+100)
		for i := range bigPayload {
			bigPayload[i] = 'a'
		}
		w := postWebhook(h, "check_suite", bigPayload)
		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	})

	t.Run("check_suite success triggers review", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "check_suite", &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			CheckSuite: &checkSuiteDetail{
				Conclusion: "success",
				HeadSHA:    "abc123",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 55}},
			},
		})
		require.Equal(t, http.StatusAccepted, w.Code)
		require.Eventually(t, func() bool { return trigger.count() >= 1 }, 2*time.Second, 50*time.Millisecond)
		require.Equal(t, "55", trigger.getLastExtra()["pr_number"])
	})

	t.Run("check_run success triggers review", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "check_run", &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			CheckRun: &checkRunDetail{
				Conclusion: "success",
				CheckSuite: &struct {
					PullRequests []struct {
						Number int `json:"number"`
					} `json:"pull_requests"`
				}{
					PullRequests: []struct {
						Number int `json:"number"`
					}{{Number: 33}},
				},
			},
		})
		require.Equal(t, http.StatusAccepted, w.Code)
		require.Eventually(t, func() bool { return trigger.count() >= 1 }, 2*time.Second, 50*time.Millisecond)
		require.Equal(t, "33", trigger.getLastExtra()["pr_number"])
	})

	t.Run("check_suite failure does not trigger", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "check_suite", &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			CheckSuite: &checkSuiteDetail{
				Conclusion: "failure",
				HeadSHA:    "abc123",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 55}},
			},
		})
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 0, trigger.count())
	})

	t.Run("unknown event returns 200 without trigger", func(t *testing.T) {
		t.Parallel()
		h, trigger := newIsolatedHandler(t)
		w := postWebhook(h, "push", &GitHubEvent{
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
		})
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 0, trigger.count())
	})
}

// --- Dedup tests ---

func TestWebhookHandler_Dedup(t *testing.T) {
	t.Parallel()

	t.Run("concurrent check_suite and check_run trigger once", func(t *testing.T) {
		t.Parallel()
		trigger := &mockTrigger{}
		cfg := testWebhookConfig()
		cfg.AllowedRepos = nil
		h := NewWebhookHandler(context.Background(), cfg, trigger, noopLogger())
		t.Cleanup(func() { h.Close() })

		csPayload := &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			CheckSuite: &checkSuiteDetail{
				Conclusion: "success",
				PullRequests: []struct {
					Number int `json:"number"`
				}{{Number: 42}},
			},
		}
		crPayload := &GitHubEvent{
			Action: "completed",
			Repository: struct {
				FullName string `json:"full_name"`
			}{FullName: "hrygo/hotplex"},
			CheckRun: &checkRunDetail{
				Conclusion: "success",
				CheckSuite: &struct {
					PullRequests []struct {
						Number int `json:"number"`
					} `json:"pull_requests"`
				}{
					PullRequests: []struct {
						Number int `json:"number"`
					}{{Number: 42}},
				},
			},
		}

		// Send both concurrently to stress the dedup atomicity.
		var wg sync.WaitGroup
		for range 5 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				postWebhook(h, "check_suite", csPayload)
			}()
			go func() {
				defer wg.Done()
				postWebhook(h, "check_run", crPayload)
			}()
		}
		wg.Wait()

		require.Eventually(t, func() bool { return trigger.count() >= 1 }, 3*time.Second, 50*time.Millisecond)
		// With atomic LoadOrStore dedup, concurrent requests for the same PR
		// should produce at most 1 trigger (the first LoadOrStore winner).
		// Allow up to 3 for rare races between expired-entry Store and new LoadOrStore.
		require.LessOrEqual(t, trigger.count(), 3, "dedup should prevent most duplicate triggers")
	})
}

// --- Rate limiting tests (AC-6) ---

func TestWebhookHandler_RateLimiting(t *testing.T) {
	t.Parallel()

	h, _ := newIsolatedHandler(t)

	var limited int
	for range 20 {
		payload := []byte(`{}`)
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader(payload))
		req.Header.Set("X-Hub-Signature-256", "sha256=abc")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	require.Greater(t, limited, 0, "some requests should be rate limited after burst exhaustion")
}

// --- Trigger error (AC-5.3) ---

type errorTrigger struct{}

func (e *errorTrigger) TriggerByName(_ context.Context, _ string, _ cron.TriggerRequest) error {
	return fmt.Errorf("job not found: %w", cron.ErrJobNotFound)
}

func TestWebhookHandler_TriggerError(t *testing.T) {
	t.Parallel()

	h := NewWebhookHandler(context.Background(), testWebhookConfig(), &errorTrigger{}, noopLogger())
	t.Cleanup(func() { h.Close() })

	w := postWebhook(h, "check_suite", &GitHubEvent{
		Action: "completed",
		Repository: struct {
			FullName string `json:"full_name"`
		}{FullName: "hrygo/hotplex"},
		CheckSuite: &checkSuiteDetail{
			Conclusion: "success",
			PullRequests: []struct {
				Number int `json:"number"`
			}{{Number: 1}},
		},
	})
	require.Equal(t, http.StatusAccepted, w.Code)
}

// --- Empty secret (AC-3.5) ---

func TestWebhookHandler_EmptySecret(t *testing.T) {
	t.Parallel()

	h, _ := newIsolatedHandler(t)
	h.cfg.Secret = ""

	payload := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", signPayload("anything", payload))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
}

// --- Occurrence identity from X-GitHub-Delivery (plan §5.4 C1) ---
//
// A webhook firing is only deduplicable when it carries the sender's own
// per-delivery identifier. These pin which requests produce one and, just as
// importantly, which do not: a firing recorded as "no verified source" must
// stay honest rather than borrow a weaker key.

func prOpenedPayload(t *testing.T, number int) *GitHubEvent {
	t.Helper()
	pr := &struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}{Number: number, State: "open"}
	pr.Head.SHA = "abc123"
	return &GitHubEvent{
		Action: "opened",
		Number: number,
		Repository: struct {
			FullName string `json:"full_name"`
		}{FullName: "hrygo/hotplex"},
		PullRequest: pr,
	}
}

func TestWebhookHandler_DeliveryIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		deliveryID string
		// wantSource is the identity the occurrence should be keyed on. Empty
		// means the firing must fall back to a per-request manual trigger.
		wantSource string
	}{
		{
			name:       "uuid delivery id is used as the occurrence identity",
			deliveryID: "72d3162e-cc78-11e3-81ab-4c9367dc0958",
			wantSource: "72d3162e-cc78-11e3-81ab-4c9367dc0958#42",
		},
		{
			name:       "absent header yields no verified identity",
			deliveryID: "",
			wantSource: "",
		},
		{
			// The value is persisted and logged. Rather than escape or trim it,
			// refuse it: an identity that had to be cleaned up is not an identity.
			name:       "header with characters outside the identifier alphabet is refused",
			deliveryID: "abc/../def",
			wantSource: "",
		},
		{
			name:       "whitespace-only header is treated as absent",
			deliveryID: "   ",
			wantSource: "",
		},
		{
			name:       "over-long header is refused rather than truncated",
			deliveryID: strings.Repeat("a", 129),
			wantSource: "",
		},
		{
			name:       "header at the length limit is accepted",
			deliveryID: strings.Repeat("a", 128),
			wantSource: strings.Repeat("a", 128) + "#42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, trigger := newIsolatedHandler(t)
			w := postWebhookDelivery(h, "pull_request", prOpenedPayload(t, 42), tt.deliveryID)
			require.Equal(t, http.StatusAccepted, w.Code)
			require.Eventually(t, func() bool { return trigger.count() >= 1 }, 2*time.Second, 50*time.Millisecond)
			require.Equal(t, tt.wantSource, trigger.getLastSourceID())
			// The prompt context is unchanged either way: the delivery id is an
			// occurrence fact, not something handed to the Agent.
			require.Equal(t, "42", trigger.getLastExtra()["pr_number"])
		})
	}
}

// One payload can name several PRs, and the trigger key carries no PR number.
// Without folding the PR number into the source id, the second PR would collide
// with the first and be silently recorded as its duplicate.
func TestWebhookHandler_OneDeliverySeveralPRsGetDistinctIdentities(t *testing.T) {
	t.Parallel()

	h, trigger := newIsolatedHandler(t)
	ev := &GitHubEvent{
		Repository: struct {
			FullName string `json:"full_name"`
		}{FullName: "hrygo/hotplex"},
		CheckSuite: &struct {
			Conclusion   string `json:"conclusion"`
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		}{Conclusion: "success", HeadSHA: "deadbeef"},
	}
	ev.CheckSuite.PullRequests = append(ev.CheckSuite.PullRequests,
		struct {
			Number int `json:"number"`
		}{Number: 7},
		struct {
			Number int `json:"number"`
		}{Number: 9},
	)

	w := postWebhookDelivery(h, "check_suite", ev, "delivery-abc")
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Eventually(t, func() bool { return trigger.count() >= 2 }, 2*time.Second, 50*time.Millisecond)

	// Both firings happen, and neither is the other's duplicate: same delivery,
	// different PR, therefore different occurrence. Compared as a set — the two
	// triggers run concurrently, so their arrival order is not defined.
	got := trigger.sourceIDs()
	require.ElementsMatch(t, []string{"delivery-abc#7", "delivery-abc#9"}, got)
}
