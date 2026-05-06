package usageprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ksred/cctrack/internal/credentials"
)

const tokenSentinel = "sk-ant-oat01-FAKE-FOR-TEST-DO-NOT-USE"

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New()
	c.endpoint = srv.URL
	// Tighten timeout so timeout-class tests don't drag.
	c.httpc.Timeout = 200 * time.Millisecond
	return c, srv
}

func validCreds() credentials.Credentials {
	return credentials.Credentials{
		AccessToken: tokenSentinel,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

// TestFetch_HTTPMatrix exercises the full HTTP/credential matrix mandated by
// F2 S2.1 evidence requirements: 200 success, 401, 403, 429, 5xx, malformed
// JSON, missing fields. Timeout and context cancellation are exercised in
// dedicated tests because they need timing control.
func TestFetch_HTTPMatrix(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		creds       credentials.Credentials
		wantErr     error
		wantFiveHr  int
		wantSevenDay int
	}{
		{
			name: "200 success with allowlisted fields",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("anthropic-beta"); got != BetaHeader {
					t.Errorf("anthropic-beta header = %q, want %q", got, BetaHeader)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+tokenSentinel {
					t.Errorf("Authorization header = %q, want bearer with sentinel", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"five_hour":{"utilization":42},"seven_day":{"utilization":73}}`)
			},
			creds:        validCreds(),
			wantFiveHr:   42,
			wantSevenDay: 73,
		},
		{
			name: "200 with extra unrelated fields, ignored",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{
					"five_hour":{"utilization":10,"alpha":"a"},
					"seven_day":{"utilization":20,"beta":"b"},
					"extra_one":"x",
					"extra_two":42,
					"extra_three":{"nested":true}
				}`)
			},
			creds:        validCreds(),
			wantFiveHr:   10,
			wantSevenDay: 20,
		},
		{
			name: "200 missing five_hour.utilization",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"five_hour":{},"seven_day":{"utilization":1}}`)
			},
			creds:   validCreds(),
			wantErr: ErrSchemaDrift,
		},
		{
			name: "200 missing seven_day.utilization",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"five_hour":{"utilization":1},"seven_day":{}}`)
			},
			creds:   validCreds(),
			wantErr: ErrSchemaDrift,
		},
		{
			name: "200 malformed JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{not json`)
			},
			creds:   validCreds(),
			wantErr: ErrSchemaDrift,
		},
		{
			name:    "401 unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
			creds:   validCreds(),
			wantErr: ErrUnauthorized,
		},
		{
			name:    "403 forbidden",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
			creds:   validCreds(),
			wantErr: ErrUnauthorized,
		},
		{
			name:    "429 rate limited",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
			creds:   validCreds(),
			wantErr: ErrRateLimited,
		},
		{
			name:    "500 internal error",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
			creds:   validCreds(),
			wantErr: ErrProviderUnavailable,
		},
		{
			name:    "503 unavailable",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
			creds:   validCreds(),
			wantErr: ErrProviderUnavailable,
		},
		{
			name: "302 redirect not followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://attacker.example/")
				w.WriteHeader(302)
			},
			creds:   validCreds(),
			wantErr: ErrProviderUnavailable,
		},
		{
			name:    "empty token",
			handler: func(w http.ResponseWriter, r *http.Request) {},
			creds:   credentials.Credentials{AccessToken: ""},
			wantErr: ErrUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.handler)
			got, err := c.Fetch(context.Background(), tc.creds)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is(%v) = true", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.FiveHourUtilizationPercent != tc.wantFiveHr {
				t.Errorf("FiveHour = %d, want %d", got.FiveHourUtilizationPercent, tc.wantFiveHr)
			}
			if got.SevenDayUtilizationPercent != tc.wantSevenDay {
				t.Errorf("SevenDay = %d, want %d", got.SevenDayUtilizationPercent, tc.wantSevenDay)
			}

			// Hard bar: error string must never contain the bearer token.
			if err != nil && strings.Contains(err.Error(), tokenSentinel) {
				t.Errorf("error string leaked token: %q", err.Error())
			}
		})
	}
}

func TestFetch_Timeout(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the test client's 200ms timeout.
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"five_hour":{"utilization":0},"seven_day":{"utilization":0}}`)
	})
	_, err := c.Fetch(context.Background(), validCreds())
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want errors.Is(ErrProviderUnavailable)", err)
	}
	if strings.Contains(err.Error(), tokenSentinel) {
		t.Errorf("error string leaked token: %q", err.Error())
	}
}

func TestFetch_ContextCancellation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"five_hour":{"utilization":0},"seven_day":{"utilization":0}}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.Fetch(ctx, validCreds())
	if err == nil {
		t.Fatalf("expected an error from canceled fetch")
	}
	// Either provider-unavailable wrap or direct context cancellation;
	// both acceptable so long as the token never leaks.
	if strings.Contains(err.Error(), tokenSentinel) {
		t.Errorf("error string leaked token: %q", err.Error())
	}
}

// TestFetch_SingleFlight verifies that two concurrent Fetch calls do not race
// the underlying transport: the second waits for the first to complete and
// the server sees exactly two sequential requests.
func TestFetch_SingleFlight(t *testing.T) {
	var hits int
	var hitMu sync.Mutex
	var hitOrder []time.Time

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hitMu.Lock()
		hits++
		hitOrder = append(hitOrder, time.Now())
		hitMu.Unlock()
		// Hold the connection long enough that overlap would be visible.
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"five_hour":{"utilization":1},"seven_day":{"utilization":2}}`)
	})

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := c.Fetch(context.Background(), validCreds())
			if err != nil {
				t.Errorf("fetch err: %v", err)
			}
		}()
	}
	wg.Wait()

	if hits != 2 {
		t.Errorf("hits = %d, want 2", hits)
	}
	if len(hitOrder) == 2 {
		gap := hitOrder[1].Sub(hitOrder[0])
		if gap < 40*time.Millisecond {
			t.Errorf("overlapping requests: gap=%v < 40ms (single-flight broken)", gap)
		}
	}
}

// TestFetch_NoBodyInError confirms the response body content does NOT appear
// in error strings on any failure-class status. We seed a recognizable
// sentinel into the response body and verify it never surfaces.
func TestFetch_NoBodyInError(t *testing.T) {
	bodySentinel := "BODY-MUST-NOT-LEAK-ZZZ"
	for _, status := range []int{401, 403, 429, 500, 503} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, bodySentinel)
			})
			_, err := c.Fetch(context.Background(), validCreds())
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), bodySentinel) {
				t.Errorf("error leaked response body: %q", err.Error())
			}
			if strings.Contains(err.Error(), tokenSentinel) {
				t.Errorf("error leaked token: %q", err.Error())
			}
		})
	}
}
