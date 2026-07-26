package turnstile_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/platform/turnstile"
)

// fakeSiteVerify stands in for Cloudflare. It records the last form it was sent
// so the request shape (secret / response / remoteip) can be asserted, and
// replies with whatever body the test hands it.
type fakeSiteVerify struct {
	*httptest.Server
	lastForm map[string]string
}

func newFakeSiteVerify(t *testing.T, status int, body string) *fakeSiteVerify {
	t.Helper()
	f := &fakeSiteVerify{lastForm: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		for k := range r.PostForm {
			f.lastForm[k] = r.PostForm.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func TestNewDisabledOnEmptySecret(t *testing.T) {
	// "Disabled" is decided once, at construction — no call site re-checks it.
	assert.Nil(t, turnstile.New(""), "an empty secret must yield no verifier")
	assert.NotNil(t, turnstile.New("secret"))
}

func TestVerifySuccess(t *testing.T) {
	fake := newFakeSiteVerify(t, http.StatusOK, `{"success":true}`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	require.NoError(t, v.Verify(context.Background(), "tok-123", "203.0.113.7"))

	assert.Equal(t, "s3cret", fake.lastForm["secret"])
	assert.Equal(t, "tok-123", fake.lastForm["response"])
	assert.Equal(t, "203.0.113.7", fake.lastForm["remoteip"])
}

func TestVerifyOmitsUnparseableRemoteIP(t *testing.T) {
	// clientIP falls back to the raw RemoteAddr when it has no port; sending
	// that as remoteip would make Cloudflare reject an otherwise valid token.
	fake := newFakeSiteVerify(t, http.StatusOK, `{"success":true}`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	require.NoError(t, v.Verify(context.Background(), "tok-123", "not-an-ip"))

	_, sent := fake.lastForm["remoteip"]
	assert.False(t, sent, "a non-IP remote address must not be forwarded")
}

func TestVerifyRejected(t *testing.T) {
	fake := newFakeSiteVerify(t, http.StatusOK,
		`{"success":false,"error-codes":["invalid-input-response","timeout-or-duplicate"]}`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	err := v.Verify(context.Background(), "spent", "203.0.113.7")

	var rejected *turnstile.RejectedError
	require.ErrorAs(t, err, &rejected, "success:false must surface as RejectedError")
	assert.Equal(t, []string{"invalid-input-response", "timeout-or-duplicate"}, rejected.Codes,
		"Cloudflare's error-codes are kept for the operator")
}

func TestVerifyRejectedWithoutCodes(t *testing.T) {
	fake := newFakeSiteVerify(t, http.StatusOK, `{"success":false}`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	var rejected *turnstile.RejectedError
	require.ErrorAs(t, v.Verify(context.Background(), "tok", ""), &rejected)
	assert.Contains(t, rejected.Error(), "rejected")
}

func TestVerifyNon200(t *testing.T) {
	fake := newFakeSiteVerify(t, http.StatusBadGateway, `nope`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	assert.Error(t, v.Verify(context.Background(), "tok", ""),
		"a non-200 from siteverify is a failure, not a pass")
}

func TestVerifyMalformedBody(t *testing.T) {
	fake := newFakeSiteVerify(t, http.StatusOK, `{"success":`)
	v := turnstile.New("s3cret", turnstile.WithEndpoint(fake.URL), turnstile.WithHTTPClient(fake.Client()))

	assert.Error(t, v.Verify(context.Background(), "tok", ""),
		"an undecodable reply must never be read as a pass")
}

func TestVerifyTransportError(t *testing.T) {
	// A dead endpoint: the upstream is unreachable, not merely unhappy.
	fake := newFakeSiteVerify(t, http.StatusOK, `{"success":true}`)
	endpoint := fake.URL
	fake.Close()

	v := turnstile.New("s3cret", turnstile.WithEndpoint(endpoint), turnstile.WithTimeout(2*time.Second))

	err := v.Verify(context.Background(), "tok", "")
	require.Error(t, err)
	var rejected *turnstile.RejectedError
	assert.False(t, errors.As(err, &rejected),
		"a transport failure is not a rejection; only the HTTP layer flattens the two")
}

func TestVerifyTimeout(t *testing.T) {
	// The bounded timeout is the point: a wedged upstream must not pin the
	// request goroutine for as long as it feels like.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); srv.Close() }()

	v := turnstile.New("s3cret", turnstile.WithEndpoint(srv.URL), turnstile.WithTimeout(50*time.Millisecond))

	start := time.Now()
	err := v.Verify(context.Background(), "tok", "")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second, "the call must be bounded by its own timeout")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
