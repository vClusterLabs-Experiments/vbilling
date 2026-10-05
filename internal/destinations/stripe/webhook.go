package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance matches Stripe's libraries.
const DefaultTolerance = 5 * time.Minute

// VerifySignature checks a Stripe-Signature header ("t=...,v1=...") against
// the raw request body. Any v1 signature may match (Stripe sends several
// while a secret is being rolled); other schemes are ignored.
func VerifySignature(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	if secret == "" {
		return errors.New("stripe webhook secret not configured")
	}
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid timestamp in Stripe-Signature: %w", err)
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return errors.New("Stripe-Signature missing t or v1")
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(ts, 0))
		if age > tolerance || age < -tolerance {
			return fmt.Errorf("Stripe-Signature timestamp outside tolerance (%s)", age.Round(time.Second))
		}
	}
	expected := sign(payload, secret, ts)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(expected)) {
			return nil
		}
	}
	return errors.New("no matching Stripe signature")
}

// SignatureHeader builds a Stripe-Signature header (tests and local tooling).
func SignatureHeader(payload []byte, secret string, t time.Time) string {
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), sign(payload, secret, t.Unix()))
}

func sign(payload []byte, secret string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
