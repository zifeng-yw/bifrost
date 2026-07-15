// Command webhooks is a minimal, dependency-free receiver for Bifrost async
// inference webhooks. It shows the one thing every receiver must get right:
// verifying the Standard Webhooks signature before trusting a delivery.
//
// Bifrost signs each delivery with HMAC-SHA256 over "{id}.{timestamp}.{body}"
// keyed with the endpoint's signing secret, and sends three headers:
//
//	webhook-id         unique id for this delivery (also the dedupe key)
//	webhook-timestamp  unix seconds the payload was signed at
//	webhook-signature  space-separated list of "v1,<base64>" signatures
//
// Retries reuse the same webhook-id, so at-least-once delivery means you can
// receive the same id more than once — dedupe on it. The signature header may
// carry multiple values (e.g. during secret rotation); accept the delivery if
// ANY of them verifies.
//
// Run it against your endpoint's secret:
//
//	WEBHOOK_SECRET=whsec_... go run .
//
// then point a Bifrost webhook endpoint at http://<this-host>:8080/webhook.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// tolerance bounds how far a delivery's timestamp may drift from now before we
// reject it as a possible replay. Standard Webhooks recommends five minutes.
const tolerance = 5 * time.Minute

// maxBodyBytes caps the request body we will read and sign over, so a
// malicious sender cannot exhaust memory. Match or exceed the largest payload
// your endpoints emit (endpoints that inline responses can be larger).
const maxBodyBytes = 1 << 20 // 1 MiB

// eventEnvelope mirrors the JSON body Bifrost delivers. Only a subset of Data
// is populated for any given event; see the field comments.
type eventEnvelope struct {
	Event     string    `json:"event"`      // "async_job.completed" | "async_job.failed"
	CreatedAt time.Time `json:"created_at"` // when this delivery was rendered
	Data      struct {
		JobID           string          `json:"job_id"`
		RequestType     string          `json:"request_type,omitempty"`
		Status          string          `json:"status"`
		StatusCode      int             `json:"status_code,omitempty"`
		ResultURL       string          `json:"result_url,omitempty"`        // GET this to fetch the result
		ResultExpiresAt *time.Time      `json:"result_expires_at,omitempty"` // after which result_url is dead
		Response        json.RawMessage `json:"response,omitempty"`          // inlined only if the endpoint opted in
		ResponseOmitted bool            `json:"response_omitted,omitempty"`  // response too large to inline; fetch it
		ResultExpired   bool            `json:"result_expired,omitempty"`    // result gone before delivery; nothing to fetch
	} `json:"data"`
}

type receiver struct {
	secret string
}

func (r *receiver) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	id := req.Header.Get("webhook-id")
	ts := req.Header.Get("webhook-timestamp")
	sig := req.Header.Get("webhook-signature")

	if err := verify(r.secret, id, ts, sig, body, time.Now()); err != nil {
		// Return 4xx so Bifrost records the failure. Do NOT echo the reason in
		// production — it can help an attacker probe your verification.
		log.Printf("rejected delivery id=%q: %v", id, err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	var envelope eventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// The signature is valid — this is a genuine Bifrost delivery. Dedupe on
	// `id` (retries reuse it) before doing any real work, then process. Here we
	// just log a summary.
	log.Printf("accepted id=%s event=%s job=%s status=%s result_url=%s",
		id, envelope.Event, envelope.Data.JobID, envelope.Data.Status, envelope.Data.ResultURL)
	if envelope.Data.ResponseOmitted {
		log.Printf("  response omitted (too large) — GET %s to fetch it", envelope.Data.ResultURL)
	}
	if envelope.Data.ResultExpired {
		log.Printf("  result expired before delivery — outcome known, result gone")
	}

	// Any 2xx tells Bifrost the delivery succeeded. Return quickly and do slow
	// work asynchronously so retries are not triggered by your own latency.
	w.WriteHeader(http.StatusNoContent)
}

// verify checks a delivery's Standard Webhooks signature. It returns nil only
// when the timestamp is within tolerance AND at least one of the signatures in
// the header matches the one we recompute from the secret.
func verify(secret, id, timestamp, signatureHeader string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signatureHeader == "" {
		return fmt.Errorf("missing webhook-id/webhook-timestamp/webhook-signature header")
	}

	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid webhook-timestamp: %w", err)
	}
	drift := now.Sub(time.Unix(secs, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return fmt.Errorf("timestamp outside tolerance (%s drift)", drift)
	}

	expected, err := sign(secret, id, secs, body)
	if err != nil {
		return err
	}

	// The header is a space-separated list of "v1,<base64>" signatures. Compare
	// every candidate against the expected one in constant time, and accept if
	// any matches. Constant-time compare avoids leaking the secret via timing.
	expectedBytes := []byte(expected)
	for _, candidate := range strings.Split(signatureHeader, " ") {
		if candidate == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), expectedBytes) == 1 {
			return nil
		}
	}
	return fmt.Errorf("no signature matched")
}

// sign recomputes the "v1,<base64>" signature for one delivery, mirroring how
// Bifrost signs it. The secret's whsec_ prefix is stripped and the remainder
// base64-decoded to obtain the raw HMAC key.
func sign(secret, id string, timestamp int64, body []byte) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("signing secret is empty")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return "", fmt.Errorf("invalid signing secret: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, timestamp)
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("set WEBHOOK_SECRET to the endpoint's signing secret (whsec_...)")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	r := &receiver{secret: secret}
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", r.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Printf("webhook receiver listening on %s (POST /webhook)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
