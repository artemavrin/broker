// Command loadtest drives synthetic load against a running broker through its
// real HTTP/WS API and reports throughput, latency percentiles and the
// producer/consumer gap (queue growth = backpressure).
//
// It bootstraps using an initiator secret: it authenticates, provisions N
// receivers via the participant API, then runs G producer goroutines
// (initiator → round-robin receivers) and one consumer per receiver. Each
// payload carries the send timestamp so consumers can measure end-to-end
// latency (enqueue → delivered).
//
// Usage:
//
//	loadtest -base http://127.0.0.1:8080 -secret <initiator-secret> \
//	  -receivers 4 -senders 16 -duration 15s -payload 256 -consumer ws
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// rcv is a provisioned receiver inbox: its id (send target) and its token.
type rcv struct {
	id, token string
}

type config struct {
	base      string
	secret    string
	receivers int
	senders   int
	duration  time.Duration
	payload   int
	batch     int
	consumer  string // "http" | "ws"
	rate      int    // total sends/sec cap across producers; 0 = unbounded
}

func main() {
	var c config
	flag.StringVar(&c.base, "base", "http://127.0.0.1:8080", "broker base URL")
	flag.StringVar(&c.secret, "secret", "", "initiator secret (required)")
	flag.IntVar(&c.receivers, "receivers", 4, "number of receiver inboxes")
	flag.IntVar(&c.senders, "senders", 16, "number of concurrent producer goroutines")
	flag.DurationVar(&c.duration, "duration", 15*time.Second, "test duration")
	flag.IntVar(&c.payload, "payload", 256, "payload size in bytes (min 8)")
	flag.IntVar(&c.batch, "batch", 32, "consumer fetch batch size")
	flag.StringVar(&c.consumer, "consumer", "http", "consumer transport: http | ws")
	flag.IntVar(&c.rate, "rate", 0, "total sends/sec cap (0 = unbounded)")
	flag.Parse()

	if c.secret == "" {
		fmt.Fprintln(os.Stderr, "error: -secret <initiator-secret> is required")
		flag.Usage()
		os.Exit(2)
	}
	if c.payload < 8 {
		c.payload = 8
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

func run(c config) error {
	httpc := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2048,
			MaxIdleConnsPerHost: 2048,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// --- bootstrap ---
	initTok, err := token(httpc, c.base, c.secret)
	if err != nil {
		return fmt.Errorf("initiator auth: %w", err)
	}
	receivers := make([]rcv, 0, c.receivers)
	for i := 0; i < c.receivers; i++ {
		id, sec, err := createReceiver(httpc, c.base, initTok)
		if err != nil {
			return fmt.Errorf("create receiver %d: %w", i, err)
		}
		tok, err := token(httpc, c.base, sec)
		if err != nil {
			return fmt.Errorf("receiver auth %d: %w", i, err)
		}
		receivers = append(receivers, rcv{id: id, token: tok})
	}
	fmt.Printf("bootstrapped: 1 initiator, %d receivers · senders=%d consumer=%s payload=%dB batch=%d duration=%s\n",
		len(receivers), c.senders, c.consumer, c.payload, c.batch, c.duration)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	deadline, dcancel := context.WithTimeout(ctx, c.duration)
	defer dcancel()

	m := newMetrics()
	var wg sync.WaitGroup

	// --- consumers ---
	for i := range receivers {
		r := receivers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.consumer == "ws" {
				consumeWS(deadline, c, httpc, r.token, m)
			} else {
				consumeHTTP(deadline, c, httpc, r.token, m)
			}
		}()
	}

	// --- producers ---
	var perSenderInterval time.Duration
	if c.rate > 0 {
		perSenderInterval = time.Duration(float64(c.senders) / float64(c.rate) * float64(time.Second))
	}
	start := time.Now()
	for s := 0; s < c.senders; s++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			produce(deadline, c, httpc, initTok, receivers, seed, perSenderInterval, m)
		}(s)
	}

	// progress ticker
	stopTick := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var last int64
		for {
			select {
			case <-stopTick:
				return
			case <-t.C:
				sent := m.sent.Load()
				delv := m.delivered.Load()
				fmt.Printf("  t=%2ds  sent=%-8d delivered=%-8d acked=%-8d  send/s=%-6d  gap=%d\n",
					int(time.Since(start).Seconds()), sent, delv, m.acked.Load(),
					sent-last, sent-delv)
				last = sent
			}
		}
	}()

	wg.Wait()
	close(stopTick)
	elapsed := time.Since(start)
	m.report(elapsed)
	return nil
}

// ---------- producer ----------

func produce(ctx context.Context, c config, hc *http.Client, tok string, receivers []rcv, seed int, interval time.Duration, m *metrics) {
	buf := make([]byte, c.payload)
	idx := seed
	var next time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		if interval > 0 {
			now := time.Now()
			if next.IsZero() {
				next = now
			}
			if d := next.Sub(now); d > 0 {
				time.Sleep(d)
			}
			next = next.Add(interval)
		}
		to := receivers[idx%len(receivers)].id
		idx++
		binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))
		t0 := time.Now()
		err := sendMsg(hc, c.base, tok, to, buf)
		lat := time.Since(t0)
		if err != nil {
			m.sendErr.Add(1)
			continue
		}
		m.sent.Add(1)
		m.sendLat.add(lat)
	}
}

// ---------- consumers ----------

func consumeHTTP(ctx context.Context, c config, hc *http.Client, tok string, m *metrics) {
	for ctx.Err() == nil {
		msgs, err := fetchHTTP(hc, c.base, tok, c.batch)
		if err != nil {
			m.fetchErr.Add(1)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if len(msgs) == 0 {
			time.Sleep(5 * time.Millisecond) // poll gap
			continue
		}
		ids := make([]int64, len(msgs))
		for i, mm := range msgs {
			ids[i] = mm.ID
			recordE2E(mm.Payload, m)
		}
		m.delivered.Add(int64(len(msgs)))
		if err := ackHTTP(hc, c.base, tok, ids); err != nil {
			m.ackErr.Add(1)
			continue
		}
		m.acked.Add(int64(len(ids)))
	}
}

func consumeWS(ctx context.Context, c config, hc *http.Client, tok string, m *metrics) {
	wsURL := "ws" + strings.TrimPrefix(c.base, "http") + "/v1/ws" // http→ws, https→wss
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		m.fetchErr.Add(1)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Drain on connect, then react to doorbells.
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "fetch", "max": c.batch})
	for ctx.Err() == nil {
		var f struct {
			Type  string `json:"type"`
			Items []struct {
				ID      int64  `json:"id"`
				Payload string `json:"payload"`
			} `json:"items"`
		}
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			return
		}
		switch f.Type {
		case "new":
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "fetch", "max": c.batch})
		case "messages":
			if len(f.Items) == 0 {
				continue
			}
			ids := make([]int64, len(f.Items))
			for i, it := range f.Items {
				ids[i] = it.ID
				if raw, err := base64.StdEncoding.DecodeString(it.Payload); err == nil {
					recordE2EBytes(raw, m)
				}
			}
			m.delivered.Add(int64(len(f.Items)))
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "ack", "ids": ids})
			m.acked.Add(int64(len(ids)))
			// There may be more than one batch of backlog; keep draining.
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "fetch", "max": c.batch})
		}
	}
}

func recordE2E(b64 string, m *metrics) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return
	}
	recordE2EBytes(raw, m)
}

func recordE2EBytes(raw []byte, m *metrics) {
	if len(raw) < 8 {
		return
	}
	ts := int64(binary.BigEndian.Uint64(raw[:8]))
	m.e2eLat.add(time.Since(time.Unix(0, ts)))
}

// ---------- API helpers ----------

func token(hc *http.Client, base, secret string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := postJSON(hc, base+"/v1/auth/token", "", map[string]string{"secret": secret}, &out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func createReceiver(hc *http.Client, base, tok string) (id, secret string, err error) {
	var out struct {
		ReceiverID     string `json:"receiver_id"`
		ReceiverSecret string `json:"receiver_secret"`
	}
	err = postJSON(hc, base+"/v1/receivers", tok, nil, &out)
	return out.ReceiverID, out.ReceiverSecret, err
}

func sendMsg(hc *http.Client, base, tok, to string, payload []byte) error {
	body := map[string]string{"to": to, "payload": base64.StdEncoding.EncodeToString(payload)}
	return postJSON(hc, base+"/v1/messages", tok, body, nil)
}

type apiMsg struct {
	ID      int64  `json:"id"`
	Payload string `json:"payload"`
}

func fetchHTTP(hc *http.Client, base, tok string, max int) ([]apiMsg, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/messages?max=%d", base, max), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("fetch status %d", resp.StatusCode)
	}
	var out []apiMsg
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func ackHTTP(hc *http.Client, base, tok string, ids []int64) error {
	return postJSON(hc, base+"/v1/messages/ack", tok, map[string]any{"ids": ids}, nil)
}

func postJSON(hc *http.Client, url, tok string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// ---------- metrics ----------

type metrics struct {
	sent, delivered, acked    atomic.Int64
	sendErr, fetchErr, ackErr atomic.Int64
	sendLat                   *latHist
	e2eLat                    *latHist
}

func newMetrics() *metrics {
	return &metrics{sendLat: newLatHist(), e2eLat: newLatHist()}
}

func (m *metrics) report(elapsed time.Duration) {
	secs := elapsed.Seconds()
	fmt.Printf("\n================ load test report ================\n")
	fmt.Printf("duration:        %.1fs\n", secs)
	fmt.Printf("sent:            %d  (%.0f/s)\n", m.sent.Load(), float64(m.sent.Load())/secs)
	fmt.Printf("delivered:       %d  (%.0f/s)\n", m.delivered.Load(), float64(m.delivered.Load())/secs)
	fmt.Printf("acked:           %d  (%.0f/s)\n", m.acked.Load(), float64(m.acked.Load())/secs)
	fmt.Printf("errors:          send=%d fetch=%d ack=%d\n", m.sendErr.Load(), m.fetchErr.Load(), m.ackErr.Load())
	fmt.Printf("backlog at end:  %d (sent - delivered)\n", m.sent.Load()-m.delivered.Load())
	fmt.Printf("\nsend latency:    %s\n", m.sendLat.summary())
	fmt.Printf("e2e latency:     %s\n", m.e2eLat.summary())
	fmt.Printf("=================================================\n")
}

// latHist is a concurrency-safe latency sample buffer with percentile output.
type latHist struct {
	mu      sync.Mutex
	samples []time.Duration
}

func newLatHist() *latHist { return &latHist{samples: make([]time.Duration, 0, 1<<16)} }

func (h *latHist) add(d time.Duration) {
	h.mu.Lock()
	h.samples = append(h.samples, d)
	h.mu.Unlock()
}

func (h *latHist) summary() string {
	h.mu.Lock()
	s := h.samples
	h.mu.Unlock()
	if len(s) == 0 {
		return "no samples"
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	p := func(q float64) time.Duration {
		idx := int(math.Ceil(q*float64(len(s)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(s) {
			idx = len(s) - 1
		}
		return s[idx].Round(time.Microsecond)
	}
	return fmt.Sprintf("n=%d  p50=%s  p95=%s  p99=%s  max=%s",
		len(s), p(0.50), p(0.95), p(0.99), s[len(s)-1].Round(time.Microsecond))
}
