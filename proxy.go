package mirageslack

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// forwardAckTimeout bounds how long we wait before returning 200 ACK to
	// Slack. Kept below Slack's 3s SLA with a safety margin.
	forwardAckTimeout = 2500 * time.Millisecond

	// forwardTotalTimeout caps the detached forward goroutine.
	forwardTotalTimeout = 30 * time.Second
)

// httpClient issues forward requests. Compression is disabled so the
// response body (and its Content-Encoding / Content-Length headers) stays
// bit-for-bit identical to what the endpoint actually sent. Without this,
// Go's default Transport silently gzip-decompresses while leaving the
// Content-Encoding: gzip header intact, which confuses the Slack side.
var httpClient = &http.Client{
	Timeout: forwardTotalTimeout,
	Transport: &http.Transport{
		DisableCompression: true,
	},
}

// forwardRequest implements the "投げるだけ投げて 3 秒で ACK" pattern.
// The forward goroutine is decoupled from the inbound request context via
// context.WithoutCancel so the endpoint can keep processing (and reply via
// response_url) even after we ACK Slack.
//
// Caveat on AWS Lambda: when running under Lambda the execution environment
// is frozen after the handler returns. The detached forward goroutine can
// therefore be paused (or lost if the sandbox is recycled) as soon as we
// write the 200 ACK. Endpoints that expect late replies must use Slack's
// response_url / trigger_id mechanisms to contact Slack directly instead of
// relying on mirage-slack to pipe a late HTTP response. In local / container
// deployments, the goroutine runs normally.
func (a *App) forwardRequest(w http.ResponseWriter, r *http.Request, bodyBytes []byte, targetURL, envName string) {
	forwardCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), forwardTotalTimeout)

	started := time.Now()
	slog.Info("forward start", "url", targetURL, "env", envName, "bytes", len(bodyBytes))

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)

	go func() {
		defer cancel()
		req, err := http.NewRequestWithContext(forwardCtx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
		if err != nil {
			errCh <- err
			return
		}
		copyHeaders(req.Header, r.Header)
		req.Header.Set("X-Mirage-Slack-Forwarded", "true")
		if envName != "" {
			req.Header.Set("X-Mirage-Slack-Env", envName)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	ackCtx, ackCancel := context.WithTimeout(r.Context(), forwardAckTimeout)
	defer ackCancel()

	select {
	case resp := <-respCh:
		defer func() {
			if err := resp.Body.Close(); err != nil {
				slog.Warn("close forward response body", "error", err)
			}
		}()
		slog.Info("forward done",
			"url", targetURL, "env", envName,
			"status", resp.StatusCode,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			slog.Warn("copy forward response body", "error", err)
		}
	case err := <-errCh:
		slog.Error("forward failed",
			"url", targetURL, "env", envName,
			"error", err,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		http.Error(w, "forward failed: "+err.Error(), http.StatusBadGateway)
	case <-ackCtx.Done():
		slog.Warn("forward ack timeout (endpoint must reply via response_url)",
			"url", targetURL, "env", envName,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		w.WriteHeader(http.StatusOK)
		go func() {
			select {
			case resp := <-respCh:
				slog.Info("forward late reply",
					"url", targetURL, "env", envName,
					"status", resp.StatusCode,
					"duration_ms", time.Since(started).Milliseconds(),
				)
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					slog.Warn("drain late forward body", "error", err)
				}
				if err := resp.Body.Close(); err != nil {
					slog.Warn("close late forward body", "error", err)
				}
			case err := <-errCh:
				slog.Error("forward late failure",
					"url", targetURL, "env", envName,
					"error", err,
					"duration_ms", time.Since(started).Milliseconds(),
				)
			}
		}()
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func skipHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
