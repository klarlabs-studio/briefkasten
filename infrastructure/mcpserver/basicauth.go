package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/klarlabs-studio/auth-go/domain"
	mcp "go.klarlabs.de/mcp"
	"go.klarlabs.de/mcp/middleware"
	"go.klarlabs.de/mcp/protocol"
	"go.klarlabs.de/mcp/transport"
)

// clientAddrKey carries the network address of the peer that made the
// request. The key type is unexported on purpose: request meta
// (protocol.SetRequestMeta) is populated from caller-supplied metadata on
// some transports, so a value stored there could be spoofed by the very
// client the rate limiter below is trying to pin down. A context value
// keyed by an unexported type can only be set by this package.
type clientAddrKey struct{}

// ForwardAuthorizationHeader returns the HTTP transport option that
// copies each request's Authorization header into the request meta,
// where auth middleware (operating on the unwrapped protocol request)
// can see it, and records the peer address for rate limiting. Pair it
// with BasicAuth when serving over HTTP.
func ForwardAuthorizationHeader() mcp.HTTPOption {
	return transport.WithRequestContextFn(authRequestContext)
}

// authRequestContext lifts the credential and the peer address off an HTTP
// request into the context the middleware chain sees.
func authRequestContext(ctx context.Context, r *http.Request) context.Context {
	if v := r.Header.Get("Authorization"); v != "" {
		ctx = protocol.SetRequestMeta(ctx, "Authorization", v)
	}
	return context.WithValue(ctx, clientAddrKey{}, peerIP(r))
}

// peerIP returns the host half of the connection's remote address.
//
// X-Forwarded-For, X-Real-IP and Forwarded are deliberately ignored. They
// are client-settable on a direct connection, so an attacker could mint a
// fresh rate-limit bucket for every request just by rotating the header —
// which would disable the limiter completely, the exact failure it exists
// to prevent. Honouring them safely requires knowing which proxies are
// trusted, and that is configuration this middleware deliberately does not
// have. The cost of the choice: behind a reverse proxy every client shares
// the proxy's address and therefore one bucket, so the argon2id CPU bound
// still holds but per-client fairness has to come from the proxy's own rate
// limiting in that deployment.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// unknownClientKey buckets requests that arrive with no peer address
// (stdio, in-process tests). Sharing one bucket is deliberate: a caller we
// cannot key must not thereby be exempt from the limit.
const unknownClientKey = "unknown"

func clientKey(ctx context.Context) string {
	if v, ok := ctx.Value(clientAddrKey{}).(string); ok && v != "" {
		return v
	}
	return unknownClientKey
}

// Attempt-limiter tuning. These are constants rather than options: the
// defaults have to be safe with no configuration, and a deployment that
// needs different numbers needs a rate limiter at its edge, not a knob here.
const (
	// authFailureThreshold is how many consecutive failures a client may
	// make before backoff starts. Generous enough that a mistyped password
	// (or a client that probes once without credentials) costs nothing.
	authFailureThreshold = 5
	// authBackoffBase is the lockout after the threshold failure; it
	// doubles with each further failure up to authBackoffMax.
	authBackoffBase = 1 * time.Second
	authBackoffMax  = 1 * time.Minute
	// authEntryTTL is how long an idle entry is kept before it may be
	// pruned. Long enough that a slow brute force cannot reset itself by
	// pausing between guesses.
	authEntryTTL = 15 * time.Minute
	// maxTrackedClients caps the tracking table so an attacker rotating
	// source addresses churns it instead of growing it without bound.
	maxTrackedClients = 4096
)

// authAttempt is one client's failure record.
type authAttempt struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// attemptLimiter bounds how often a single client may make the server spend
// an argon2id derivation. Entries are created only by failures, so a client
// that authenticates successfully never occupies a slot, and the table stays
// bounded at maxTrackedClients (see evictLocked).
type attemptLimiter struct {
	mu      sync.Mutex
	clients map[string]*authAttempt
	now     func() time.Time // swappable in tests
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{
		clients: make(map[string]*authAttempt),
		now:     time.Now,
	}
}

// allow reports whether key may spend a password verification right now.
// A locked-out client costs a map lookup instead of an argon2id derivation,
// which is the whole point: the check runs before any hashing. Being
// refused does not extend the lockout, so a flood of refused requests
// cannot hold a client out indefinitely.
func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.clients[key]
	if !ok {
		return true
	}
	now := l.now()
	if now.Before(entry.lockedUntil) {
		return false
	}
	entry.lastSeen = now
	return true
}

// recordFailure counts a rejected attempt and arms the backoff once the
// client crosses the threshold.
func (l *attemptLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.clients[key]
	if !ok {
		l.evictLocked(now)
		entry = &authAttempt{}
		l.clients[key] = entry
	}
	entry.failures++
	entry.lastSeen = now
	if entry.failures >= authFailureThreshold {
		entry.lockedUntil = now.Add(backoffFor(entry.failures))
	}
}

// recordSuccess clears a client's history. One client's failures never
// touch another's entry, so a correct credential is never slowed down by
// somebody else's guessing.
func (l *attemptLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, key)
}

// evictLocked frees a slot when the table is full, so the map is hard
// bounded at maxTrackedClients. It samples a handful of entries rather than
// scanning: taking the true least-recently-seen entry would be O(n) work on
// a path an address-rotating attacker controls. Go randomises map iteration,
// so the sample is unbiased; an entry idle past authEntryTTL is taken
// immediately, otherwise the oldest of the sample goes. Evicting (rather
// than refusing to track new clients once full) matters — a table that
// could be filled to leave newcomers untracked would be a bypass.
//
// The residual: an attacker with many source addresses can churn the table
// and buy a fresh budget per address. Nothing keyed on the client can stop
// that; bound it at the network edge.
//
// Callers must hold l.mu.
func (l *attemptLimiter) evictLocked(now time.Time) {
	if len(l.clients) < maxTrackedClients {
		return
	}
	const sample = 8
	var (
		victim string
		oldest time.Time
		found  bool
		seen   int
	)
	for k, entry := range l.clients {
		if now.Sub(entry.lastSeen) >= authEntryTTL && !now.Before(entry.lockedUntil) {
			delete(l.clients, k)
			return
		}
		if !found || entry.lastSeen.Before(oldest) {
			victim, oldest, found = k, entry.lastSeen, true
		}
		seen++
		if seen >= sample {
			break
		}
	}
	if found {
		delete(l.clients, victim)
	}
}

// backoffFor returns the lockout duration for a given failure count,
// doubling per failure past the threshold and saturating at authBackoffMax.
func backoffFor(failures int) time.Duration {
	shift := failures - authFailureThreshold
	if shift < 0 {
		shift = 0
	}
	if shift > 16 { // guard the shift against overflow
		return authBackoffMax
	}
	d := authBackoffBase << uint(shift)
	if d <= 0 || d > authBackoffMax {
		return authBackoffMax
	}
	return d
}

// basicAuthExemptMethods are the MCP handshake methods that stay reachable
// without credentials so clients can negotiate before authenticating. They
// perform no password verification, so they are not a hashing DoS vector
// and are not rate limited here.
var basicAuthExemptMethods = map[string]bool{
	"initialize": true,
	"ping":       true,
}

// errUnauthorized is the single rejection used by every failure path —
// missing header, wrong username, wrong password, rate limited. Callers
// cannot tell which, so neither the existence of a username nor the fact
// that a client is being throttled leaks.
func errUnauthorized() error {
	return protocol.NewUnauthorized("valid basic credentials required")
}

// basicAuthenticator holds the expected credential and the per-client
// attempt limiter shared by every request the middleware sees.
type basicAuthenticator struct {
	wantUser []byte
	verify   func(string) error
	limiter  *attemptLimiter
}

// BasicAuth returns MCP middleware that requires every request to carry
// an Authorization: Basic credential matching the given username and
// argon2id password hash (a PHC string, e.g. from auth-go HashPassword).
//
// Verification is constant time on both the username and the password
// (auth-go's PasswordHash.Verify). Authentication failures are opaque —
// the same rejection regardless of which part was wrong. The MCP
// handshake methods (initialize, ping) stay reachable so clients can
// negotiate before presenting credentials; every tool, resource, and
// prompt call is gated. MCP clients are not browsers, so there is no
// cookie/session handshake: the credential rides each request, which is
// what HTTP basic auth is built for.
//
// Attempts are bounded per client before any hashing happens. Argon2id is
// deliberately expensive, so verifying every request that carries a header
// would let anyone who can reach the endpoint burn CPU at will and guess
// passwords without limit. After authFailureThreshold consecutive failures
// a client is refused for an exponentially growing window (1s doubling to
// 1m) without the hash being computed at all. The client is keyed by the
// connection's peer address — see peerIP for why X-Forwarded-For is not
// honoured — and successful authentication clears that client's history,
// so one client's guessing never slows another's valid credential.
//
// mcp-go v1.19 removed its in-library auth, so this is a self-contained
// middleware: it rejects with a JSON-RPC Unauthorized error rather than
// relying on a framework authenticator.
func BasicAuth(username string, hash domain.PasswordHash) mcp.Middleware {
	return newBasicAuthenticator(username, hash.Verify).middleware()
}

// newBasicAuthenticator builds the authenticator around a password
// verifier. Taking the verifier as a function keeps the argon2id work
// injectable, which is how the tests assert that a rate-limited request
// never reaches it.
func newBasicAuthenticator(username string, verify func(string) error) *basicAuthenticator {
	return &basicAuthenticator{
		wantUser: []byte(username),
		verify:   verify,
		limiter:  newAttemptLimiter(),
	}
}

func (a *basicAuthenticator) middleware() mcp.Middleware {
	return func(next middleware.HandlerFunc) middleware.HandlerFunc {
		return func(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
			if basicAuthExemptMethods[req.Method] {
				return next(ctx, req)
			}
			key := clientKey(ctx)
			// Bound attempts first: past the threshold this returns
			// before a single argon2id block is touched.
			if !a.limiter.allow(key) {
				return nil, errUnauthorized()
			}
			if !a.credentialsValid(ctx) {
				a.limiter.recordFailure(key)
				return nil, errUnauthorized()
			}
			a.limiter.recordSuccess(key)
			return next(ctx, req)
		}
	}
}

// credentialsValid parses the request's Authorization metadata and verifies
// it against the expected username and password hash in constant time. The
// verification runs outside the limiter's lock so concurrent requests are
// not serialised behind one another's argon2id derivation.
func (a *basicAuthenticator) credentialsValid(ctx context.Context) bool {
	auth := protocol.GetRequestMeta(ctx, "Authorization")
	if auth == "" {
		auth = protocol.GetRequestMeta(ctx, "authorization")
	}
	const prefix = "Basic "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, prefix))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	// Check both halves unconditionally so a known username is not
	// distinguishable from an unknown one by timing.
	userOK := subtle.ConstantTimeCompare([]byte(user), a.wantUser) == 1
	passErr := a.verify(pass)
	return userOK && passErr == nil
}
