package api

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/webhookforward"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func (h *Handler) buildWebhookForwardResponse(r *http.Request) (webhookforward.ForwardResult, error) {
	if r.Method != http.MethodPost {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: "Unsupported method for /webhook/forward"}
	}
	if !isLoopbackRequest(r) {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusForbidden, message: "Webhook forwarding is limited to loopback callers"}
	}
	forwarder := h.webhookForwarder
	if runtimeWithForwarder, ok := any(h.context.Runtime).(interface {
		WebhookForwarder() looperdruntime.WebhookForwarder
	}); ok {
		if current := runtimeWithForwarder.WebhookForwarder(); current != nil {
			forwarder = current
		}
	}
	if forwarder == nil {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Webhook forwarding is not configured"}
	}
	if runtimeWithWebhook, ok := any(h.context.Runtime).(interface {
		WebhookStatus() looperdruntime.WebhookStatus
	}); ok {
		status := runtimeWithWebhook.WebhookStatus()
		if !status.Enabled {
			return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusServiceUnavailable, message: "webhook runtime is disabled; deliveries are not being processed"}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	eventType := r.Header.Get("X-GitHub-Event")
	result, err := forwarder.Forward(r.Context(), webhookforward.DeliveryRequest{DeliveryID: deliveryID, EventType: eventType, Payload: body})
	if err != nil {
		status := http.StatusBadRequest
		code := pkgapi.ErrorCodeValidationFailed
		message := err.Error()
		// Post-gate admission refusal (race after outer AllowMutations) is temporary
		// unavailability, not a bad delivery payload.
		if errors.Is(err, webhookforward.ErrAdmissionRefused) {
			status = http.StatusServiceUnavailable
			code = pkgapi.ErrorCodeServiceUnavailable
		} else {
			lower := strings.ToLower(message)
			if strings.Contains(lower, "not configured") {
				status = http.StatusInternalServerError
				code = pkgapi.ErrorCodeInternalError
			} else if strings.Contains(lower, "queue is full") {
				status = http.StatusServiceUnavailable
			}
		}
		return webhookforward.ForwardResult{}, apiError{code: code, status: status, message: message}
	}
	if (strings.EqualFold(result.Status, "accepted") || result.WorkItems > 0) && any(h.context.Runtime) != nil {
		runtimeWithWebhook, ok := any(h.context.Runtime).(interface{ RecordWebhookDelivery(string, string) })
		if ok {
			runtimeWithWebhook.RecordWebhookDelivery(eventType, deliveryID)
		}
	}
	return result, nil
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *Handler) buildWebhookStatusResponse() looperdruntime.WebhookStatus {
	if runtimeWithWebhook, ok := any(h.context.Runtime).(interface {
		WebhookStatus() looperdruntime.WebhookStatus
	}); ok {
		return runtimeWithWebhook.WebhookStatus()
	}
	return looperdruntime.WebhookStatus{
		Enabled:                     h.context.Config.Webhook.Enabled,
		Mode:                        h.context.Config.Webhook.Mode,
		FallbackPollIntervalSeconds: h.context.Config.Webhook.FallbackPollIntervalSeconds,
		ListenerPath:                "/webhook/forward",
		EndpointURL:                 strings.TrimRight(serverBaseURL(h.context.Config.Server), "/") + "/webhook/forward",
		TunnelPublicBaseURL:         strings.TrimRight(strings.TrimSpace(h.context.Config.Webhook.PublicBaseURL), "/"),
		DegradedReasons:             []string{},
		RecentOutcomes:              []looperdruntime.WebhookRecentOutcome{},
		Forwarders:                  []looperdruntime.WebhookForwarderState{},
	}
}

func summarizeWebhookStatus(status looperdruntime.WebhookStatus) statusWebhook {
	running := 0
	for _, forwarder := range status.Forwarders {
		if forwarder.Running {
			running++
		}
	}
	return statusWebhook{
		Enabled:                     status.Enabled,
		EndpointURL:                 status.EndpointURL,
		FallbackPollIntervalSeconds: status.FallbackPollIntervalSeconds,
		Degraded:                    status.Degraded,
		DegradedReasons:             append([]string{}, status.DegradedReasons...),
		ConfiguredForwarders:        len(status.Forwarders),
		RunningForwarders:           running,
	}
}

func serverBaseURL(cfg config.ServerConfig) string {
	if cfg.BaseURL != nil && strings.TrimSpace(*cfg.BaseURL) != "" {
		return strings.TrimSpace(*cfg.BaseURL)
	}
	return fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
}
