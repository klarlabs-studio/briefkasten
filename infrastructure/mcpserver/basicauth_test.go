package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klarlabs-studio/auth-go/domain"
	"go.klarlabs.de/mcp/protocol"
)

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// invoke runs the middleware chain against a request carrying the given
// Authorization header and reports whether next ran.
func invoke(t *testing.T, hash domain.PasswordHash, method, authorization string) (called bool, err error) {
	t.Helper()
	mw := BasicAuth("alice", hash)
	next := func(_ context.Context, _ *protocol.Request) (*protocol.Response, error) {
		called = true
		return &protocol.Response{}, nil
	}
	ctx := context.Background()
	if authorization != "" {
		ctx = protocol.SetRequestMeta(ctx, "Authorization", authorization)
	}
	_, err = mw(next)(ctx, &protocol.Request{Method: method})
	return called, err
}

func TestBasicAuthMiddleware(t *testing.T) {
	hash, err := domain.HashPassword("s3cret", domain.DefaultArgon2idParams())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"valid credentials", basicHeader("alice", "s3cret"), true},
		{"wrong password", basicHeader("alice", "nope"), false},
		{"wrong username", basicHeader("mallory", "s3cret"), false},
		{"no header", "", false},
		{"bearer scheme", "Bearer sometoken", false},
		{"malformed base64", "Basic !!!", false},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called, err := invoke(t, hash, "tools/list", tc.header)
			if called != tc.want {
				t.Errorf("next called = %v, want %v (err %v)", called, tc.want, err)
			}
			if !tc.want {
				var perr *protocol.Error
				if !errors.As(err, &perr) || perr.Code != protocol.CodeUnauthorized {
					t.Errorf("want unauthorized protocol error, got %v", err)
				}
			}
		})
	}
}

func TestBasicAuthMiddlewareSkipsHandshake(t *testing.T) {
	hash, err := domain.HashPassword("s3cret", domain.DefaultArgon2idParams())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"initialize", "ping"} {
		called, err := invoke(t, hash, method, "")
		if !called || err != nil {
			t.Errorf("%s should pass without credentials, called=%v err=%v", method, called, err)
		}
	}
}

// countingVerifier stands in for the argon2id derivation so a test can
// assert how many times it ran. Counting is the only reliable way to prove
// a rate-limited request never reached the hash — timing would be a guess.
type countingVerifier struct {
	calls atomic.Int64
}

func (c *countingVerifier) verify(pass string) error {
	c.calls.Add(1)
	if pass != "s3cret" {
		return errors.New("password mismatch")
	}
	return nil
}

// authCtx builds a request context as the HTTP transport would: peer
// address plus Authorization metadata.
func authCtx(addr, authorization string) context.Context {
	ctx := context.WithValue(context.Background(), clientAddrKey{}, addr)
	if authorization != "" {
		ctx = protocol.SetRequestMeta(ctx, "Authorization", authorization)
	}
	return ctx
}

// limitedHarness wires an authenticator with a counting verifier and a
// handler that records whether the request got through.
type limitedHarness struct {
	auth     *basicAuthenticator
	verifier *countingVerifier
	calls    func() int64
	call     func(ctx context.Context) (bool, error)
}

func newLimitedHarness(t *testing.T) *limitedHarness {
	t.Helper()
	v := &countingVerifier{}
	a := newBasicAuthenticator("alice", v.verify)
	h := &limitedHarness{auth: a, verifier: v, calls: v.calls.Load}
	mw := a.middleware()
	h.call = func(ctx context.Context) (bool, error) {
		called := false
		next := func(_ context.Context, _ *protocol.Request) (*protocol.Response, error) {
			called = true
			return &protocol.Response{}, nil
		}
		_, err := mw(next)(ctx, &protocol.Request{Method: "tools/list"})
		return called, err
	}
	return h
}

func wantUnauthorized(t *testing.T, err error) *protocol.Error {
	t.Helper()
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.CodeUnauthorized {
		t.Fatalf("want unauthorized protocol error, got %v", err)
	}
	return perr
}

// TestBasicAuthStopsHashingAfterRepeatedFailures is the CPU-exhaustion
// guard: a client that keeps guessing must stop costing the server an
// argon2id derivation per request.
func TestBasicAuthStopsHashingAfterRepeatedFailures(t *testing.T) {
	h := newLimitedHarness(t)
	const attempts = 50
	for i := 0; i < attempts; i++ {
		called, err := h.call(authCtx("10.0.0.1", basicHeader("alice", "nope")))
		if called {
			t.Fatalf("attempt %d passed the gate", i)
		}
		_ = wantUnauthorized(t, err)
	}
	if got := h.calls(); got != authFailureThreshold {
		t.Errorf("password verifications = %d, want %d (limiter must refuse before hashing)", got, authFailureThreshold)
	}
	// Even a correct credential from the locked-out client is refused, and
	// still without hashing.
	called, err := h.call(authCtx("10.0.0.1", basicHeader("alice", "s3cret")))
	if called {
		t.Error("locked-out client authenticated")
	}
	_ = wantUnauthorized(t, err)
	if got := h.calls(); got != authFailureThreshold {
		t.Errorf("password verifications = %d after lockout, want %d", got, authFailureThreshold)
	}
}

// TestBasicAuthLimitIsPerClient proves a valid credential is not punished
// for somebody else's brute force.
func TestBasicAuthLimitIsPerClient(t *testing.T) {
	h := newLimitedHarness(t)
	for i := 0; i < 20; i++ {
		if called, _ := h.call(authCtx("10.0.0.1", basicHeader("alice", "nope"))); called {
			t.Fatal("attacker passed the gate")
		}
	}
	called, err := h.call(authCtx("10.0.0.2", basicHeader("alice", "s3cret")))
	if !called || err != nil {
		t.Errorf("innocent client rejected while another is limited: called=%v err=%v", called, err)
	}
}

// TestBasicAuthSuccessClearsHistory: a client that authenticates gets its
// failure budget back, so an occasional typo never accumulates into a
// lockout for a legitimate user.
func TestBasicAuthSuccessClearsHistory(t *testing.T) {
	h := newLimitedHarness(t)
	for round := 0; round < 3; round++ {
		for i := 0; i < authFailureThreshold-1; i++ {
			if called, _ := h.call(authCtx("10.0.0.3", basicHeader("alice", "nope"))); called {
				t.Fatalf("round %d attempt %d passed the gate", round, i)
			}
		}
		called, err := h.call(authCtx("10.0.0.3", basicHeader("alice", "s3cret")))
		if !called || err != nil {
			t.Fatalf("round %d: valid credential rejected: called=%v err=%v", round, called, err)
		}
	}
	// Every attempt reached the verifier: the client never crossed the
	// threshold because each success reset it.
	if got, want := h.calls(), int64(3*authFailureThreshold); got != want {
		t.Errorf("password verifications = %d, want %d", got, want)
	}
}

// TestBasicAuthBackoffExpiresAndGrows drives the limiter with a fake clock:
// the lockout lifts on its own, and each further failure lengthens it.
func TestBasicAuthBackoffExpiresAndGrows(t *testing.T) {
	h := newLimitedHarness(t)
	now := time.Now()
	h.auth.limiter.now = func() time.Time { return now }

	bad := func() { _, _ = h.call(authCtx("10.0.0.4", basicHeader("alice", "nope"))) }
	for i := 0; i < authFailureThreshold; i++ {
		bad()
	}
	if got := h.calls(); got != authFailureThreshold {
		t.Fatalf("verifications = %d before backoff, want %d", got, authFailureThreshold)
	}
	// Locked: no further hashing while the window is open.
	now = now.Add(authBackoffBase - time.Millisecond)
	bad()
	if got := h.calls(); got != authFailureThreshold {
		t.Fatalf("verification ran during the backoff window (calls=%d)", got)
	}
	// Window elapsed: the attempt is evaluated again, and failing extends
	// the lockout to twice the base.
	now = now.Add(2 * time.Millisecond)
	bad()
	if got := h.calls(); got != authFailureThreshold+1 {
		t.Fatalf("verifications = %d after backoff expiry, want %d", got, authFailureThreshold+1)
	}
	now = now.Add(authBackoffBase)
	bad()
	if got := h.calls(); got != authFailureThreshold+1 {
		t.Fatalf("verification ran inside the doubled backoff window (calls=%d)", got)
	}
	now = now.Add(2 * authBackoffBase)
	bad()
	if got := h.calls(); got != authFailureThreshold+2 {
		t.Fatalf("verifications = %d after the doubled backoff, want %d", got, authFailureThreshold+2)
	}
	// Once the window passes, a valid credential works again and wipes the
	// history: the client gets its full budget back.
	now = now.Add(authBackoffMax)
	if called, err := h.call(authCtx("10.0.0.4", basicHeader("alice", "s3cret"))); !called || err != nil {
		t.Fatalf("valid credential rejected after backoff: called=%v err=%v", called, err)
	}
	for i := 0; i < authFailureThreshold-1; i++ {
		bad()
	}
	if called, err := h.call(authCtx("10.0.0.4", basicHeader("alice", "s3cret"))); !called || err != nil {
		t.Fatalf("history not cleared by success: called=%v err=%v", called, err)
	}
}

func TestBackoffFor(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, authBackoffBase},
		{authFailureThreshold, authBackoffBase},
		{authFailureThreshold + 1, 2 * authBackoffBase},
		{authFailureThreshold + 2, 4 * authBackoffBase},
		{authFailureThreshold + 100, authBackoffMax},
		{1 << 20, authBackoffMax},
	}
	for _, tc := range cases {
		got := backoffFor(tc.failures)
		if got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
		if got > authBackoffMax || got <= 0 {
			t.Errorf("backoffFor(%d) = %v, outside (0, %v]", tc.failures, got, authBackoffMax)
		}
	}
}

// TestBasicAuthRateLimitedResponseIsOpaque: a throttled client learns
// nothing about which half of the credential was wrong, or that it is being
// throttled at all.
func TestBasicAuthRateLimitedResponseIsOpaque(t *testing.T) {
	h := newLimitedHarness(t)
	var before *protocol.Error
	for i := 0; i < authFailureThreshold; i++ {
		_, err := h.call(authCtx("10.0.0.5", basicHeader("alice", "nope")))
		before = wantUnauthorized(t, err)
	}
	headers := map[string]string{
		"known user, wrong password":   basicHeader("alice", "nope"),
		"unknown user, wrong password": basicHeader("mallory", "nope"),
		"unknown user, right password": basicHeader("mallory", "s3cret"),
		"known user, right password":   basicHeader("alice", "s3cret"),
		"no credentials":               "",
	}
	for name, header := range headers {
		_, err := h.call(authCtx("10.0.0.5", header))
		perr := wantUnauthorized(t, err)
		if perr.Code != before.Code || perr.Message != before.Message || perr.Data != nil {
			t.Errorf("%s: rejection differs from the pre-limit one: %+v vs %+v", name, perr, before)
		}
	}
}

// TestBasicAuthHandshakeSurvivesLockout: the exempt methods do no hashing,
// so limiting must not take them down.
func TestBasicAuthHandshakeSurvivesLockout(t *testing.T) {
	h := newLimitedHarness(t)
	for i := 0; i < 20; i++ {
		_, _ = h.call(authCtx("10.0.0.6", basicHeader("alice", "nope")))
	}
	mw := h.auth.middleware()
	for _, method := range []string{"initialize", "ping"} {
		called := false
		next := func(_ context.Context, _ *protocol.Request) (*protocol.Response, error) {
			called = true
			return &protocol.Response{}, nil
		}
		_, err := mw(next)(authCtx("10.0.0.6", ""), &protocol.Request{Method: method})
		if !called || err != nil {
			t.Errorf("%s blocked by the limiter: called=%v err=%v", method, called, err)
		}
	}
}

// TestAttemptLimiterBounded: an attacker rotating source addresses must not
// grow the tracking table without limit.
func TestAttemptLimiterBounded(t *testing.T) {
	l := newAttemptLimiter()
	for i := 0; i < maxTrackedClients*3; i++ {
		l.recordFailure(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	l.mu.Lock()
	size := len(l.clients)
	l.mu.Unlock()
	if size > maxTrackedClients {
		t.Errorf("tracked clients = %d, want <= %d", size, maxTrackedClients)
	}
}

// TestAttemptLimiterEvictsWhenFull covers both eviction branches — an entry
// idle past the TTL, and (all entries fresh) the oldest of the sample. In
// both cases the table stays capped and the newcomer is tracked, so a full
// table is never a way to become untracked.
func TestAttemptLimiterEvictsWhenFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		idle time.Duration
	}{
		{"entries idle past the TTL", authEntryTTL + time.Minute},
		{"all entries fresh", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newAttemptLimiter()
			now := time.Now()
			l.now = func() time.Time { return now }
			for i := 0; i < maxTrackedClients; i++ {
				l.recordFailure(fmt.Sprintf("10.0.%d.%d", i>>8&0xff, i&0xff))
			}
			now = now.Add(tc.idle)
			l.recordFailure("192.0.2.1")
			l.mu.Lock()
			size := len(l.clients)
			_, tracked := l.clients["192.0.2.1"]
			l.mu.Unlock()
			if size > maxTrackedClients {
				t.Errorf("tracked clients = %d, want <= %d", size, maxTrackedClients)
			}
			if !tracked {
				t.Error("new client untracked on a full table")
			}
		})
	}
}

// TestBasicAuthConcurrent exercises the limiter from many goroutines; run
// with -race, this is the concurrency-safety assertion.
func TestBasicAuthConcurrent(t *testing.T) {
	h := newLimitedHarness(t)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			addr := fmt.Sprintf("10.1.0.%d", g%8)
			for i := 0; i < 50; i++ {
				pass := "nope"
				if i%3 == 0 {
					pass = "s3cret"
				}
				_, _ = h.call(authCtx(addr, basicHeader("alice", pass)))
			}
		}(g)
	}
	wg.Wait()
	l := h.auth.limiter
	l.mu.Lock()
	size := len(l.clients)
	l.mu.Unlock()
	if size > maxTrackedClients {
		t.Errorf("tracked clients = %d, want <= %d", size, maxTrackedClients)
	}
}

// TestClientKeyFallsBackToSharedBucket: a request with no peer address is
// still limited rather than exempt.
func TestClientKeyFallsBackToSharedBucket(t *testing.T) {
	if got := clientKey(context.Background()); got != unknownClientKey {
		t.Errorf("clientKey without an address = %q, want %q", got, unknownClientKey)
	}
	if got := clientKey(context.WithValue(context.Background(), clientAddrKey{}, "")); got != unknownClientKey {
		t.Errorf("clientKey with an empty address = %q, want %q", got, unknownClientKey)
	}
	if got := clientKey(authCtx("198.51.100.7", "")); got != "198.51.100.7" {
		t.Errorf("clientKey = %q, want the peer address", got)
	}
}

// TestPeerIPIgnoresForwardedHeaders is the trap this middleware must not
// fall into: if X-Forwarded-For were honoured on a direct connection, an
// attacker could rotate it and get an unlimited number of fresh rate-limit
// buckets — one per request — defeating the limiter entirely.
func TestPeerIPIgnoresForwardedHeaders(t *testing.T) {
	cases := []struct {
		name   string
		addr   string
		header map[string]string
		want   string
	}{
		{"host and port", "203.0.113.9:51234", nil, "203.0.113.9"},
		{"ipv6", "[2001:db8::1]:443", nil, "2001:db8::1"},
		{"no port", "203.0.113.9", nil, "203.0.113.9"},
		{
			"spoofed forwarding headers",
			"203.0.113.9:51234",
			map[string]string{
				"X-Forwarded-For": "10.9.9.9",
				"X-Real-IP":       "10.8.8.8",
				"Forwarded":       "for=10.7.7.7",
			},
			"203.0.113.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
			r.RemoteAddr = tc.addr
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			if got := peerIP(r); got != tc.want {
				t.Errorf("peerIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestContextCarriesCredentialAndPeer: the transport hook feeds both
// the credential and the rate-limit key, and the key comes from the
// connection rather than any header the caller can write.
func TestRequestContextCarriesCredentialAndPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
	r.RemoteAddr = "203.0.113.9:51234"
	r.Header.Set("Authorization", basicHeader("alice", "s3cret"))
	r.Header.Set("X-Forwarded-For", "10.9.9.9")

	ctx := authRequestContext(context.Background(), r)
	if got := protocol.GetRequestMeta(ctx, "Authorization"); got != basicHeader("alice", "s3cret") {
		t.Errorf("Authorization meta = %q", got)
	}
	if got := clientKey(ctx); got != "203.0.113.9" {
		t.Errorf("rate-limit key = %q, want the peer address", got)
	}

	// No Authorization header: the peer is still keyed, so a client that
	// hammers the endpoint without credentials is limited too.
	bare := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
	bare.RemoteAddr = "203.0.113.10:1"
	ctx = authRequestContext(context.Background(), bare)
	if got := protocol.GetRequestMeta(ctx, "Authorization"); got != "" {
		t.Errorf("Authorization meta = %q, want empty", got)
	}
	if got := clientKey(ctx); got != "203.0.113.10" {
		t.Errorf("rate-limit key = %q, want the peer address", got)
	}
}
