package mirageslack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// maxRequestAge is the replay window for Slack signing verification.
const maxRequestAge = 5 * time.Minute

// verifySlackSignature validates the X-Slack-Signature header against the
// body using the v0 scheme described in
// https://api.slack.com/authentication/verifying-requests-from-slack
func verifySlackSignature(h http.Header, body []byte, secret string) bool {
	ts := h.Get("X-Slack-Request-Timestamp")
	sig := h.Get("X-Slack-Signature")
	if ts == "" || sig == "" || secret == "" {
		return false
	}

	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(tsInt, 0)).Abs() > maxRequestAge {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(sig))
}
