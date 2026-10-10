package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ----------------------------------------------------------- speed test ---
// The test runs on the machine the panel runs on, through the router, against
// the same Ookla servers speedtest.net picks. Running it on the router itself
// was tried first and can't work at these speeds: traffic the router makes
// itself goes through its 2-core CPU (softirq-bound at ~350 Mbit/s), while
// traffic it forwards for LAN devices is hardware-offloaded. So a router-side
// test read ~270 Mbit/s on a line that speedtest.net measured at 446.
//
// Each direction runs speedStreams HTTP streams and samples the bytes moved
// every speedTick. The estimate is computed like Ookla's: the slowest 30% and
// fastest 10% of the samples are dropped (TCP ramp-up, stalls, bursts) and the
// rest averaged. The phase stops once that estimate moved less than speedTol
// two seconds in a row (at least speedMin, at most speedMax) - per-sample
// rates on a mobile link swing ±30%, the trimmed mean settles in a few
// seconds, which saves data over a fixed-length test.

const (
	speedStreams = 16
	speedMin     = 5 * time.Second
	speedMax     = 10 * time.Second
	speedTol     = 0.04
	speedTick    = 250 * time.Millisecond
)

type speedServer struct {
	URL     string  `json:"-"` // base, e.g. http://host:8080
	Sponsor string  `json:"sponsor"`
	Name    string  `json:"name"`
	Ookla   bool    `json:"-"`
	Latency float64 `json:"-"`
}

func (s speedServer) downURL(n int) string {
	if s.Ookla {
		return fmt.Sprintf("%s/download?nocache=%d&size=25000000", s.URL, n)
	}
	return s.URL + "/__down?bytes=25000000"
}

func (s speedServer) upURL(n int) string {
	if s.Ookla {
		return fmt.Sprintf("%s/upload?nocache=%d", s.URL, n)
	}
	return s.URL + "/__up"
}

var cloudflareServer = speedServer{URL: "http://speed.cloudflare.com", Sponsor: "Cloudflare", Name: "nearest"}

// speedState is what the page polls while a test runs: the phase, the live
// rate for the speedometer, and the result once it's done.
type speedState struct {
	Running  bool           `json:"running"`
	Phase    string         `json:"phase"`               // server, ping, download, upload, done, error
	Mbps     float64        `json:"mbps"`                // live, the phase running now
	DownMbps float64        `json:"down_mbps,omitempty"` // final, once download is done
	UpMbps   float64        `json:"up_mbps,omitempty"`   // final, once upload is done
	PingMs   float64        `json:"ping_ms,omitempty"`
	Progress float64        `json:"progress"` // 0..1 within the phase
	Server   *speedServer   `json:"server,omitempty"`
	Client   string         `json:"client,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
}

var (
	speedMu  sync.Mutex
	speedCur speedState
)

func speedStatus() speedState {
	speedMu.Lock()
	defer speedMu.Unlock()
	return speedCur
}

func setSpeed(f func(*speedState)) {
	speedMu.Lock()
	f(&speedCur)
	speedMu.Unlock()
}

// speedClient keeps a fresh connection pool per test and never reuses the
// panel's other HTTP state.
func speedClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: speedStreams * 2,
		DisableCompression:  true,
	}}
}

// startSpeedTest starts a test in the background (or leaves the running one
// alone) and returns the state to poll.
func startSpeedTest() speedState {
	speedMu.Lock()
	defer speedMu.Unlock()
	if speedCur.Running {
		return speedCur
	}
	host, _ := os.Hostname()
	speedCur = speedState{Running: true, Phase: "server", Client: host}
	go func() {
		res, err := runSpeedTest()
		setSpeed(func(s *speedState) {
			s.Running, s.Mbps, s.Progress = false, 0, 1
			if err != nil {
				s.Phase, s.Error = "error", err.Error()
				return
			}
			s.Phase, s.Result = "done", res
		})
	}()
	return speedCur
}

func runSpeedTest() (map[string]any, error) {
	c := speedClient()
	srv := pickSpeedServer(c)
	setSpeed(func(s *speedState) { s.Server, s.Phase = &srv, "ping" })

	ping, jitter, err := speedPing(srv)
	if err != nil {
		return nil, fmt.Errorf("Can't reach the speed test server %s: %v", srv.Sponsor, err)
	}
	down, dBytes := speedPhase(c, srv, "download")
	setSpeed(func(s *speedState) { s.DownMbps = roundMbps(down) })
	up, uBytes := speedPhase(c, srv, "upload")
	setSpeed(func(s *speedState) { s.UpMbps = roundMbps(up) })
	if down == 0 && up == 0 {
		return nil, fmt.Errorf("Speed test moved no data - is the internet up?")
	}
	return map[string]any{
		"server":    srv.Sponsor + " · " + srv.Name,
		"ping_ms":   math.Round(ping*10) / 10,
		"jitter_ms": math.Round(jitter*10) / 10,
		"down_mbps": roundMbps(down),
		"up_mbps":   roundMbps(up),
		"bytes":     dBytes + uBytes,
	}, nil
}

func roundMbps(bps float64) float64 { return math.Round(bps*8/1e4) / 100 }

// pickSpeedServer asks speedtest.net for the servers nearest to this
// connection and takes the one with the lowest latency, like speedtest.net
// does; Cloudflare if that list can't be had.
func pickSpeedServer(c *http.Client) speedServer {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.speedtest.net/api/js/servers?engine=js&limit=5", nil)
	resp, err := c.Do(req)
	if err != nil {
		return cloudflareServer
	}
	defer resp.Body.Close()
	var list []struct {
		Host    string `json:"host"`
		Sponsor string `json:"sponsor"`
		Name    string `json:"name"`
	}
	if json.NewDecoder(resp.Body).Decode(&list) != nil || len(list) == 0 {
		return cloudflareServer
	}
	var cands []speedServer
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, l := range list {
		s := speedServer{URL: "http://" + l.Host, Sponsor: l.Sponsor, Name: l.Name, Ookla: true}
		wg.Add(1)
		go func() {
			defer wg.Done()
			best := math.Inf(1)
			for i := 0; i < 3; i++ {
				if d, err := tcpRTT(s, 2*time.Second); err == nil && d < best {
					best = d
				}
			}
			if !math.IsInf(best, 1) {
				s.Latency = best
				mu.Lock()
				cands = append(cands, s)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(cands) == 0 {
		return cloudflareServer
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Latency < cands[j].Latency })
	return cands[0]
}

// speedPing times 10 TCP connects to the server - the pure round trip. Ping
// is the median (a mobile link throws the odd 150 ms outlier, which a mean
// would carry), jitter the mean difference between consecutive ones.
func speedPing(srv speedServer) (ping, jitter float64, err error) {
	var times []float64
	var lastErr error
	for i := 0; i < 10; i++ {
		d, err := tcpRTT(srv, 3*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		times = append(times, d)
		ping = median(times)
		setSpeed(func(s *speedState) { s.PingMs, s.Progress = math.Round(ping*10)/10, float64(i+1)/10 })
		time.Sleep(100 * time.Millisecond)
	}
	if len(times) == 0 {
		return 0, 0, lastErr
	}
	for i := 1; i < len(times); i++ {
		jitter += math.Abs(times[i] - times[i-1])
	}
	if len(times) > 1 {
		jitter /= float64(len(times) - 1)
	}
	return ping, jitter, nil
}

// tcpRTT times one TCP connect to the server (ms).
func tcpRTT(srv speedServer, timeout time.Duration) (float64, error) {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return 0, err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "80")
	}
	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, err
	}
	d := float64(time.Since(t0).Microseconds()) / 1000
	conn.Close()
	return d, nil
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

// trimmedRate is the Ookla-style estimate over per-tick rates: drop the
// slowest 30% and the fastest 10%, average the rest.
func trimmedRate(rates []float64) float64 {
	if len(rates) == 0 {
		return 0
	}
	r := append([]float64(nil), rates...)
	sort.Float64s(r)
	lo, hi := len(r)*3/10, len(r)-len(r)/10
	if lo >= hi {
		lo, hi = 0, len(r)
	}
	sum := 0.0
	for _, v := range r[lo:hi] {
		sum += v
	}
	return sum / float64(hi-lo)
}

// zeroReader is an endless upload body that counts what was sent.
type zeroReader struct{ n *atomic.Int64 }

func (z zeroReader) Read(p []byte) (int, error) {
	clear(p)
	z.n.Add(int64(len(p)))
	return len(p), nil
}

// speedPhase runs one direction and returns its rate (bytes/s) and the bytes
// it moved. The live rate for the speedometer is the last half second's.
func speedPhase(c *http.Client, srv speedServer, phase string) (float64, int64) {
	setSpeed(func(s *speedState) { s.Phase, s.Mbps, s.Progress = phase, 0, 0 })
	ctx, cancel := context.WithTimeout(context.Background(), speedMax+2*time.Second)
	defer cancel()
	var moved atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < speedStreams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ctx.Err() == nil; n++ {
				var req *http.Request
				if phase == "download" {
					req, _ = http.NewRequestWithContext(ctx, "GET", srv.downURL(i*1000+n), nil)
				} else {
					// a fixed-size body per request, like speedtest.net's
					body := io.LimitReader(zeroReader{&moved}, 25000000)
					req, _ = http.NewRequestWithContext(ctx, "POST", srv.upURL(i*1000+n), body)
					req.ContentLength = 25000000
					req.Header.Set("Content-Type", "application/octet-stream")
				}
				resp, err := c.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						time.Sleep(200 * time.Millisecond)
					}
					continue
				}
				if phase == "download" {
					buf := make([]byte, 64<<10)
					for {
						k, err := resp.Body.Read(buf)
						moved.Add(int64(k))
						if err != nil {
							break
						}
					}
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
				}
				resp.Body.Close()
			}
		}(i)
	}

	start := time.Now()
	type sample struct {
		t time.Duration
		b int64
	}
	var hist []sample
	var rates []float64 // bytes/s per tick
	var est float64
	var moves []float64
	perSec := int(time.Second / speedTick)
	tick := time.NewTicker(speedTick)
	for range tick.C {
		now := sample{time.Since(start), moved.Load()}
		prev := sample{}
		if len(hist) > 0 {
			prev = hist[len(hist)-1]
		}
		hist = append(hist, now)
		rates = append(rates, float64(now.b-prev.b)/(now.t-prev.t).Seconds())
		// The speedometer shows the same running estimate the result is
		// computed from, so it glides towards the final number instead of
		// jumping with every half-second burst; before there are enough
		// samples for it, the plain average so far.
		live := float64(now.b) / now.t.Seconds()
		if now.t >= time.Second {
			live = trimmedRate(rates)
		}
		setSpeed(func(s *speedState) {
			s.Mbps = roundMbps(live)
			s.Progress = math.Min(1, now.t.Seconds()/speedMax.Seconds())
		})
		if len(hist)%perSec == 0 {
			e := trimmedRate(rates)
			if est > 0 {
				moves = append(moves, math.Abs(e-est)/est)
			}
			est = e
			k := len(moves)
			if now.t >= speedMin && k >= 2 && moves[k-1] < speedTol && moves[k-2] < speedTol {
				break
			}
		}
		if now.t >= speedMax {
			break
		}
	}
	tick.Stop()
	cancel()
	wg.Wait()
	mean := trimmedRate(rates)
	log.Printf("speed test %s: %.1f Mbit/s over %.1f s, %d MB", phase, mean*8/1e6, time.Since(start).Seconds(), moved.Load()>>20)
	return mean, moved.Load()
}

// handleSpeedTest: POST starts a test, GET polls it.
func handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ok(w, startSpeedTest())
		return
	}
	ok(w, speedStatus())
}
