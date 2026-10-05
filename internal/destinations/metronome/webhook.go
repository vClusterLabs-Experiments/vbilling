package metronome

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultTolerance is the replay window Metronome recommends.
const DefaultTolerance = 5 * time.Minute

// VerifySignature checks Metronome-Webhook-Signature, which is
// hex(HMAC-SHA256(secret, X-Metronome-Date + "\n" + raw body)). Pass the
// X-Metronome-Date header (Date is accepted by Metronome only for backward
// compatibility because proxies rewrite it). tolerance <= 0 skips the
// freshness check.
func VerifySignature(body []byte, date, signature, secret string, tolerance time.Duration, now time.Time) error {
	if secret == "" {
		return errors.New("metronome webhook secret not configured")
	}
	if date == "" || signature == "" {
		return errors.New("missing X-Metronome-Date or Metronome-Webhook-Signature")
	}
	if tolerance > 0 {
		sent, err := http.ParseTime(date)
		if err != nil {
			return fmt.Errorf("invalid X-Metronome-Date: %w", err)
		}
		if age := now.Sub(sent); age > tolerance || age < -tolerance {
			return fmt.Errorf("webhook date outside tolerance (%s)", age.Round(time.Second))
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(date))
	mac.Write([]byte("\n"))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(strings.TrimSpace(signature)))) {
		return errors.New("metronome signature mismatch")
	}
	return nil
}

// Sign produces a signature for tests and local tooling.
func Sign(body []byte, date, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(date))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
