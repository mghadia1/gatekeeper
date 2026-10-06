package ratelimit

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// storeUnderTest is a store plus a way to move its clock.
type storeUnderTest struct {
	Store
	advance func(time.Duration)
}

func memoryStore(limit Limit) storeUnderTest {
	c := &clock{t: time.Unix(1_000_000, 0)}
	return storeUnderTest{NewMemoryStore(limit, c.now), c.advance}
}

// miniredisStore runs the real Lua script in an in-process Redis whose clock
// the test controls.
func miniredisStore(t *testing.T, limit Limit) (storeUnderTest, *miniredis.Miniredis) {
	t.Helper()
	m := miniredis.RunT(t)
	now := time.Unix(1_000_000, 0)
	m.SetTime(now)
	client := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return storeUnderTest{NewRedisStore(client, limit), func(d time.Duration) {
		now = now.Add(d)
		m.SetTime(now)
		m.FastForward(d) // expire keys as real time would
	}}, m
}

// eachStore runs a behaviour test against both stores: they must agree.
func eachStore(t *testing.T, limit Limit, test func(t *testing.T, s storeUnderTest)) {
	t.Run("memory", func(t *testing.T) { test(t, memoryStore(limit)) })
	t.Run("redis", func(t *testing.T) {
		s, _ := miniredisStore(t, limit)
		test(t, s)
	})
}

func take(t *testing.T, s Store, key string) Decision {
	t.Helper()
	d, err := s.Take(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBurstThenRefusalWithRetryAfter(t *testing.T) {
	eachStore(t, Limit{Rate: 10, Burst: 3}, func(t *testing.T, s storeUnderTest) {
		for i, wantRemaining := range []int{2, 1, 0} {
			d := take(t, s, "client")
			if !d.Allowed || d.Remaining != wantRemaining {
				t.Fatalf("request %d = %+v, want allowed with %d remaining", i+1, d, wantRemaining)
			}
		}
		d := take(t, s, "client")
		if d.Allowed || d.RetryAfter != 100*time.Millisecond {
			t.Fatalf("request 4 = %+v, want refused with RetryAfter 100ms (1 token at 10/s)", d)
		}
	})
}

func TestFractionalRefill(t *testing.T) {
	eachStore(t, Limit{Rate: 10, Burst: 1}, func(t *testing.T, s storeUnderTest) {
		take(t, s, "c") // empty the bucket
		s.advance(50 * time.Millisecond)
		d := take(t, s, "c")
		if d.Allowed || d.RetryAfter != 50*time.Millisecond {
			t.Fatalf("after 50 ms = %+v, want refused with 50 ms to go (half a token so far)", d)
		}
		s.advance(50 * time.Millisecond)
		if d := take(t, s, "c"); !d.Allowed {
			t.Fatalf("after 100 ms = %+v, want allowed: the half token was kept", d)
		}
	})
}

func TestAClientAtExactlyTheRateIsNeverRefused(t *testing.T) {
	eachStore(t, Limit{Rate: 10, Burst: 1}, func(t *testing.T, s storeUnderTest) {
		for i := range 50 {
			if d := take(t, s, "steady"); !d.Allowed {
				t.Fatalf("request %d of a client at exactly 10/s was refused: %+v", i+1, d)
			}
			s.advance(100 * time.Millisecond)
		}
	})
}

func TestClientsHaveSeparateBuckets(t *testing.T) {
	eachStore(t, Limit{Rate: 1, Burst: 1}, func(t *testing.T, s storeUnderTest) {
		take(t, s, "a")
		if take(t, s, "a").Allowed {
			t.Fatal("a's second request was allowed")
		}
		if !take(t, s, "b").Allowed {
			t.Fatal("b was refused because of a's usage")
		}
	})
}

func TestBucketRefillsNoFurtherThanBurst(t *testing.T) {
	eachStore(t, Limit{Rate: 100, Burst: 2}, func(t *testing.T, s storeUnderTest) {
		take(t, s, "c")
		s.advance(time.Hour)
		allowed := 0
		for range 10 {
			if take(t, s, "c").Allowed {
				allowed++
			}
		}
		if allowed != 2 {
			t.Fatalf("after an hour idle, %d requests allowed at once, want the burst (2)", allowed)
		}
	})
}

func TestConcurrentRequestsCannotOverspend(t *testing.T) {
	eachStore(t, Limit{Rate: 0.001, Burst: 25}, func(t *testing.T, s storeUnderTest) {
		var allowed atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 10 {
					if d, err := s.Take(context.Background(), "shared"); err == nil && d.Allowed {
						allowed.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if got := allowed.Load(); got != 25 {
			t.Fatalf("%d of 200 concurrent requests allowed, want exactly the burst (25)", got)
		}
	})
}

func TestMemoryStoreDropsIdleBuckets(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	s := NewMemoryStore(Limit{Rate: 10, Burst: 10}, c.now)
	for i := range 100 {
		take(t, s, "client-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if s.Len() != 100 {
		t.Fatalf("%d buckets, want 100", s.Len())
	}
	c.advance(sweepEvery) // long enough to refill completely, and to sweep
	take(t, s, "newcomer")
	if s.Len() != 1 {
		t.Fatalf("%d buckets after the idle ones refilled, want 1 (the newcomer)", s.Len())
	}
}

func TestRedisKeysExpireOnceTheBucketWouldBeFull(t *testing.T) {
	s, m := miniredisStore(t, Limit{Rate: 10, Burst: 20}) // full after 2 s
	take(t, s, "c")
	ttl := m.TTL("gatekeeper:rl:c")
	if ttl < 2*time.Second || ttl > 4*time.Second {
		t.Fatalf("bucket TTL %v, want about the 2 s refill time plus a margin", ttl)
	}
	s.advance(ttl + time.Millisecond)
	if m.Exists("gatekeeper:rl:c") {
		t.Fatal("the bucket key outlived its TTL")
	}
}

func TestRedisStoreReportsAnUnreachableServer(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()
	_, err := NewRedisStore(client, Limit{Rate: 1, Burst: 1}).Take(context.Background(), "c")
	if err == nil || !strings.Contains(err.Error(), "rate limit store") {
		t.Fatalf("Take with Redis down = %v, want a rate limit store error", err)
	}
}

func TestClientKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // must not be trusted
	if got := ClientKey(req, "X-API-Key"); got != "ip:203.0.113.7" {
		t.Errorf("without a key: %q, want the connection's IP", got)
	}
	req.Header.Set("X-API-Key", "secret-key-123")
	got := ClientKey(req, "X-API-Key")
	if !strings.HasPrefix(got, "key:") || strings.Contains(got, "secret") || len(got) != len("key:")+16 {
		t.Errorf("with a key: %q, want key: plus a 16-character hash, never the key itself", got)
	}
	if ClientKey(req, "") != "ip:203.0.113.7" {
		t.Error("with no key header configured, the IP should be used")
	}
}

// The integration test runs the script on a real Redis, where TIME, HSET
// with several fields and number formatting are the real thing rather than
// miniredis's versions. It needs GATEKEEPER_TEST_REDIS_URL: skipped without
// it locally, an error without it in CI.
func TestRealRedisSharedAcrossReplicas(t *testing.T) {
	url := os.Getenv("GATEKEEPER_TEST_REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("GATEKEEPER_TEST_REDIS_URL must be set in CI")
		}
		t.Skip("GATEKEEPER_TEST_REDIS_URL not set; `make test-redis` runs this against Compose's Redis")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	limit := Limit{Rate: 1, Burst: 10}
	// Two clients and two stores: two gateway replicas.
	replicas := make([]*RedisStore, 2)
	for i := range replicas {
		client := redis.NewClient(opts)
		t.Cleanup(func() { _ = client.Close() })
		replicas[i] = NewRedisStore(client, limit)
	}
	key := "integration-" + time.Now().Format("150405.000000000")

	var allowed atomic.Int32
	var wg sync.WaitGroup
	started := time.Now()
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				d, err := replicas[i%2].Take(context.Background(), key)
				if err != nil {
					t.Error(err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	// 200 requests across two replicas: the burst, plus whatever refilled
	// while they ran (1 per second).
	maxAllowed := int32(10 + int(elapsed.Seconds()) + 1)
	if got := allowed.Load(); got < 10 || got > maxAllowed {
		t.Fatalf("%d of 200 allowed across 2 replicas in %v, want 10..%d: one shared limit", got, elapsed, maxAllowed)
	}
}
