package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/cron"
)

// GitHubEvent represents the common fields of GitHub webhook payloads.
// PullRequest fields are used by the fork-fallback path in extractPRs
// for pull_request events (opened/synchronize/reopened on open, non-draft PRs).
// Origin PRs are primarily triggered via check_suite/check_run (CI-only, #662).
type GitHubEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Number      int `json:"number"`
	PullRequest *struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	CheckSuite *struct {
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	CheckRun *struct {
		Conclusion string `json:"conclusion"`
		CheckSuite *struct {
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
	} `json:"check_run"`
}

// JobTrigger triggers a named cron job with optional extra context.
type JobTrigger interface {
	TriggerByName(ctx context.Context, jobName string, req cron.TriggerRequest) error
}

// githubDeliveryID is the sender's per-delivery identifier, used as the
// occurrence's idempotency key.
//
// GitHub mints one GUID per delivery and reuses it when redelivering the same
// event (the REST API exposes it as `guid` alongside a `redelivery` flag), so it
// is exactly the "verified source/event ID" the occurrence ledger needs: a
// redelivery resolves to the run that already happened instead of starting a
// second one.
//
// The header is not covered by X-Hub-Signature-256 — that signature covers the
// body only. It is still trustworthy here because this function is only reached
// after signature verification, so reaching it already required the secret; a
// replayed capture carries its original ID and is deduplicated by it.
func githubDeliveryID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if id == "" || len(id) > 128 {
		return ""
	}
	// The value is persisted as an occurrence's source id and appears in logs,
	// so anything outside the identifier alphabet is refused rather than
	// trimmed or escaped. GitHub sends a UUID; the wider set keeps this working
	// if the format ever changes.
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return ""
		}
	}
	return id
}

const maxConcurrentTriggers = 5

// triggerDedupCooldown prevents duplicate triggers for the same PR within this window.
const triggerDedupCooldown = 300 * time.Second

// WebhookHandler handles incoming GitHub webhook requests.
type WebhookHandler struct {
	cfg       config.WebhookConfig
	trigger   JobTrigger
	limiter   *rate.Limiter
	sem       chan struct{} // bounds concurrent trigger goroutines
	log       *slog.Logger
	baseCtx   context.Context    // gateway lifecycle context for async goroutines
	cancel    context.CancelFunc // cancels in-flight triggers on shutdown
	dedup     sync.Map           // "repo#pr_number" -> time.Time; cooldown-based dedup
	wg        sync.WaitGroup     // tracks in-flight trigger + sweeper goroutines
	closeOnce sync.Once          // ensures Close is idempotent
}

// NewWebhookHandler creates a new GitHub webhook handler.
// baseCtx should be the gateway lifecycle context so in-flight triggers
// are cancelled on graceful shutdown.
func NewWebhookHandler(baseCtx context.Context, cfg config.WebhookConfig, trigger JobTrigger, log *slog.Logger) *WebhookHandler {
	ctx, cancel := context.WithCancel(baseCtx)
	h := &WebhookHandler{
		cfg:     cfg,
		trigger: trigger,
		limiter: rate.NewLimiter(2, 10), // 2 req/s, burst 10
		sem:     make(chan struct{}, maxConcurrentTriggers),
		log:     log.With("component", "webhook"),
		baseCtx: ctx,
		cancel:  cancel,
	}
	h.wg.Add(1)
	go h.dedupSweeper()
	return h
}

// Close cancels in-flight trigger goroutines, stops the dedup sweeper,
// and waits for all goroutines to finish. Safe to call multiple times.
func (h *WebhookHandler) Close() {
	h.closeOnce.Do(func() {
		h.cancel()
		h.wg.Wait()
		h.dedup.Range(func(key, _ any) bool {
			h.dedup.Delete(key)
			return true
		})
	})
}

// dedupSweeper periodically purges stale dedup entries.
func (h *WebhookHandler) dedupSweeper() {
	defer h.wg.Done()
	ticker := time.NewTicker(triggerDedupCooldown * 2)
	defer ticker.Stop()
	for {
		select {
		case <-h.baseCtx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			h.dedup.Range(func(key, val any) bool {
				if t, _ := val.(time.Time); now.Sub(t) > triggerDedupCooldown {
					h.dedup.Delete(key)
				}
				return true
			})
		}
	}
}

// ServeHTTP handles incoming GitHub webhook HTTP requests.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Method check
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. Rate limit
	if !h.limiter.Allow() {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	// 3. Read body with size limit
	body := io.LimitReader(r.Body, h.cfg.MaxBodySize+1)
	payload, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Reject if body exceeds limit (we read one extra byte to detect overflow).
	if int64(len(payload)) > h.cfg.MaxBodySize {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// 4. HMAC-SHA256 signature verification
	sig := r.Header.Get("X-Hub-Signature-256")
	if !h.verifySignature(payload, sig) {
		h.log.Warn("webhook: invalid signature", "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 5. Ping event (GitHub sends this on webhook creation)
	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "ping" {
		h.log.Info("webhook: ping received", "repo", extractRepoFromPing(payload))
		w.WriteHeader(http.StatusOK)
		return
	}

	// 6. Parse event
	var event GitHubEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		h.log.Warn("webhook: invalid JSON payload", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 7. Repository filter
	if len(h.cfg.AllowedRepos) > 0 && !slices.Contains(h.cfg.AllowedRepos, event.Repository.FullName) {
		h.log.Warn("webhook: unexpected repo", "repo", event.Repository.FullName)
		w.WriteHeader(http.StatusOK) // 200 to prevent GitHub retries
		return
	}

	// 8. Extract PRs that need review
	prNumbers := h.extractPRs(eventType, &event)
	if len(prNumbers) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 9. Async trigger with dedup and bounded concurrency
	jobName := h.cfg.TargetJobName
	repo := event.Repository.FullName
	// One X-GitHub-Delivery covers the whole payload, but a single payload can
	// name several PRs, and the occurrence's trigger key does not contain the
	// PR number. Folding the PR number in is what keeps those from colliding
	// on one delivery id. The number comes from the request body, which the
	// signature above already verified, so it carries the same trust as the
	// delivery id itself.
	deliveryID := githubDeliveryID(r)
	for _, prNum := range prNumbers {
		// Dedup: atomically check and mark to prevent TOCTOU race when
		// check_suite and check_run arrive near-simultaneously.
		prKey := repo + "#" + strconv.Itoa(prNum)
		now := time.Now()
		if actual, loaded := h.dedup.LoadOrStore(prKey, now); loaded {
			if t, _ := actual.(time.Time); now.Sub(t) < triggerDedupCooldown {
				h.log.Info("webhook: skipping duplicate trigger", "pr", prNum)
				continue
			}
			h.dedup.Store(prKey, now)
		}

		select {
		case h.sem <- struct{}{}:
			h.wg.Add(1)
			go func(n int) {
				defer func() { <-h.sem; h.wg.Done() }()
				ctx, cancel := context.WithTimeout(h.baseCtx, 5*time.Minute)
				defer cancel()
				sourceID := deliveryID
				if sourceID != "" {
					sourceID += "#" + strconv.Itoa(n)
				}
				err := h.trigger.TriggerByName(ctx, jobName, cron.TriggerRequest{
					Extra: map[string]string{
						"trigger":   "webhook",
						"pr_number": strconv.Itoa(n),
					},
					VerifiedEventID: sourceID,
				})
				if err != nil {
					if errors.Is(err, cron.ErrJobDisabled) || errors.Is(err, cron.ErrJobNotFound) {
						h.log.Warn("webhook: trigger skipped", "pr", n, "err", err)
					} else {
						h.log.Error("webhook: trigger failed", "pr", n, "err", err)
					}
				} else {
					h.log.Info("webhook: triggered review", "pr", n)
				}
			}(prNum)
		default:
			h.log.Warn("webhook: trigger concurrency limit reached, skipping", "pr", prNum)
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// verifySignature verifies the HMAC-SHA256 signature in constant time.
func (h *WebhookHandler) verifySignature(payload []byte, sig string) bool {
	if h.cfg.Secret == "" || sig == "" {
		return false
	}
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	sigHex := strings.TrimPrefix(sig, "sha256=")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.cfg.Secret))
	mac.Write(payload)
	expected := mac.Sum(nil)

	return hmac.Equal(sigBytes, expected)
}

// extractPRs returns PR numbers that need review based on event type and action.
//
// Two trigger paths:
//  1. check_suite/check_run (CI-only, preferred): triggers only after CI succeeds,
//     ensuring review sees final code. This is the primary path for origin PRs.
//  2. pull_request (fork fallback): triggers on opened/synchronize/reopened for
//     open, non-draft PRs. This covers fork PRs where check_suite events from
//     the fork's CI are not forwarded to the origin repo's webhook (GitHub limitation).
//     Dedup ensures that if a check_suite also arrives (origin PRs), only the first
//     trigger wins.
func (h *WebhookHandler) extractPRs(eventType string, e *GitHubEvent) []int {
	switch eventType {
	case "check_suite":
		if e.CheckSuite == nil || e.CheckSuite.Conclusion != "success" {
			return nil
		}
		return extractPRNumbers(e.CheckSuite.PullRequests)
	case "check_run":
		if e.CheckRun == nil || e.CheckRun.Conclusion != "success" || e.CheckRun.CheckSuite == nil {
			return nil
		}
		return extractPRNumbers(e.CheckRun.CheckSuite.PullRequests)
	case "pull_request":
		// Fork PR fallback: trigger on new commits or new/open PRs.
		// Skip closed/merged/draft PRs, missing SHA, and non-actionable events.
		if e.PullRequest == nil || e.PullRequest.Head.SHA == "" ||
			e.PullRequest.State != "open" || e.PullRequest.Draft {
			return nil
		}
		switch e.Action {
		case "opened", "synchronize", "reopened", "ready_for_review":
			return []int{e.PullRequest.Number}
		}
	}
	return nil
}

// extractPRNumbers extracts PR numbers from the inline PullRequests structs.
func extractPRNumbers(prs []struct {
	Number int `json:"number"`
}) []int {
	numbers := make([]int, 0, len(prs))
	for _, pr := range prs {
		numbers = append(numbers, pr.Number)
	}
	return numbers
}

// extractRepoFromPing attempts to extract the repository full name from a ping payload.
func extractRepoFromPing(payload []byte) string {
	var ping struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &ping); err == nil {
		return ping.Repository.FullName
	}
	return ""
}
