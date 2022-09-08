package httpretry_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ladydascalie/go-httpretry"
)

// TestNewClient tests the client and it's ability to retry requests.
func TestNewClient(t *testing.T) {
	t.Run("passing in an optional logger works", func(t *testing.T) {
		var buf bytes.Buffer

		// no timestamps for stable output.
		logger := log.New(&buf, "TEST_LOGGER: ", 0)

		client := httpretry.NewClient(httpretry.WithLogger(logger))

		ts := httptest.NewServer(okHandler(t))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		scanner := bufio.NewScanner(&buf)
		for scanner.Scan() {
			text := scanner.Text()
			if !strings.HasPrefix(text, logger.Prefix()) {
				t.Fatalf("expected line to start with %s, but got: %s", logger.Prefix(), text)
			}
		}
	})

	t.Run("retry policy options are set", func(t *testing.T) {
		client := httpretry.NewClient(
			httpretry.WithRetryPolicy(&httpretry.RetryPolicy{
				Retries:     5,
				WaitTime:    time.Second,
				BackoffMode: httpretry.BackoffModeExponential,
			}),
		)
		if _, ok := client.Transport.(*httpretry.RetryTransport); !ok {
			t.Errorf("expected transport to be %T, but got: %T", &httpretry.RetryTransport{}, client.Transport)
		}
	})

	t.Run("timeout option is set", func(t *testing.T) {
		timeout := time.Second
		client := httpretry.NewClient(
			httpretry.WithTimeout(timeout),
		)
		if client.Timeout != timeout {
			t.Errorf("expected timeout to be %s, but was: %s", timeout, client.Timeout)
		}
	})

	t.Run("cookie jar option is set", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		client := httpretry.NewClient(
			httpretry.WithCookieJar(jar),
		)
		if client.Jar != jar {
			t.Error("cookie jar did not match")
		}
	})

	t.Run("redirect method is set", func(t *testing.T) {
		client := httpretry.NewClient(
			httpretry.WithCheckRedirect(func(_ *http.Request, _ []*http.Request) error {
				t.Log("this client does not follow redirects")
				return http.ErrUseLastResponse
			}),
		)

		ts := httptest.NewServer(redirectLoop(t))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err != nil {
			t.Fatalf("expected no error but got: %v", err)
		}
	})
}

func TestClientRetries(t *testing.T) {
	t.Parallel()

	policy := &httpretry.RetryPolicy{
		Retries:       2,
		WaitTime:      50 * time.Millisecond,
		BackoffMode:   httpretry.BackoffModeLinear,
		ResponseCheck: httpretry.DefaultResponseChecker,
	}
	client := httpretry.NewClient(
		httpretry.WithRetryPolicy(policy),
	)

	t.Run("client follows retry policy if requests time out", func(t *testing.T) {
		ts := httptest.NewServer(timeoutHandler(t))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			// we expect an error here
			t.Fatal("expected error, but got nil")
		}

		var retryError httpretry.RequestError
		if !errors.As(err, &retryError) {
			t.Fatalf("expected %T, but got: %T", retryError, err)
		}
	})

	t.Run("explicitly failed http request is retried", func(t *testing.T) {
		var tries int

		ts := httptest.NewServer(failHandler(t, &tries))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			// we expect an error here
			t.Fatal("expected error, but got nil")
		}

		if tries != policy.Retries {
			t.Fatalf("expected %d retries, but got %d", policy.Retries, tries)
		}

		var retryError httpretry.RequestError
		if !errors.As(err, &retryError) {
			t.Fatalf("expected %T, but got: %T", retryError, err)
		}
	})
}

func TestStatusChecker(t *testing.T) {
	t.Parallel()

	// use defaults except for shorter wait time.
	policy := httpretry.NewDefaultRetryPolicy()
	policy.WaitTime = 100 * time.Millisecond

	client := httpretry.NewClient(httpretry.WithRetryPolicy(policy))

	t.Run("internal error triggers a retry", func(t *testing.T) {
		var tries int
		ts := httptest.NewServer(failHandler(t, &tries))

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			// we expect an error here
			t.Fatal("expected error, but got nil")
		}

		if tries != policy.Retries {
			t.Fatalf("expected %d retries, but got %d", policy.Retries, tries)
		}

		if retryError, ok := errors.Unwrap(err).(httpretry.RequestError); !ok {
			t.Fatalf("expected %T, but got: %T", httpretry.RequestError{}, retryError)
		}
	})

	t.Run("too many requests error triggers a retry", func(t *testing.T) {
		var tries int
		ts := httptest.NewServer(tooManyRequestsHandler(t, &tries))

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			// we expect an error here
			t.Fatal("expected error, but got nil")
		}

		var retryError httpretry.RequestError
		if !errors.As(err, &retryError) {
			t.Fatalf("expected %T, but got: %T", retryError, err)
		}
	})

	t.Run("retry-after header request is honoured", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1") // retry after 1 second
			w.WriteHeader(http.StatusServiceUnavailable)
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.Do(req)
		if err == nil {
			// we expect an error here
			t.Fatal("expected error, but got nil")
		}

		var retryError httpretry.RequestError
		if !errors.As(err, &retryError) {
			t.Fatalf("expected %T, but got: %T", retryError, err)
		}
	})

	t.Run("unix retry-after header is honoured", func(t *testing.T) {
		expectedRetryAfter := 123

		response := new(http.Response)
		response.StatusCode = http.StatusTooManyRequests
		response.Header = make(http.Header)
		response.Header.Set("Retry-After", fmt.Sprint(expectedRetryAfter))

		shouldRetry, retryAfter := httpretry.DefaultResponseChecker(response)

		t.Log(shouldRetry, retryAfter)

		if !shouldRetry {
			t.Fatal("shouldRetry must be true")
		}
		if retryAfter != 123*time.Second {
			t.Fatalf("expected retryAfter to be %d, but got %d", expectedRetryAfter, retryAfter)
		}
	})
	t.Run("date retry-after header is honoured", func(t *testing.T) {
		// sometime in the future, about 1000 hours ahead.
		ts := time.Now().Add(1e3 * time.Hour)

		// setup an expectation.
		expectedRetryAfter := time.Duration(ts.Unix()-time.Now().Unix()) * time.Second

		response := new(http.Response)
		response.StatusCode = http.StatusTooManyRequests
		response.Header = make(http.Header)
		response.Header.Set("Retry-After", ts.Format(time.RFC1123))

		shouldRetry, retryAfter := httpretry.DefaultResponseChecker(response)
		if !shouldRetry {
			t.Fatal("shouldRetry must be true")
		}

		// check if we are within the expected error tolerance
		if !closeEnough(expectedRetryAfter, retryAfter) {
			t.Fatalf("expected retry-after to be %d, but got %d", expectedRetryAfter, retryAfter)
		}
	})
}

func TestRequestError_Error(t *testing.T) {
	type fields struct {
		Err      error
		Response *http.Response
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{
			name:   "base error is returned when no additional intormation is provided",
			fields: fields{},
			want:   "max retries exceeded",
		},
		{
			name: "response code is added when it is provided",
			fields: fields{
				Response: &http.Response{Status: http.StatusText(http.StatusOK)},
			},
			want: "max retries exceeded: OK",
		},
		{
			name: "error is added when it is provided",
			fields: fields{
				Err: errors.New("oops"),
			},
			want: "max retries exceeded: oops",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := httpretry.RequestError{
				Err:      tt.fields.Err,
				Response: tt.fields.Response,
			}
			if got := e.Error(); got != tt.want {
				t.Errorf("RequestError.Error() = %v, want %v", got, tt.want)
			}
		})
	}
}

func closeEnough(t1, t2 time.Duration) bool {
	x, y := float64(t1), float64(t2)

	return math.Abs(x-y) < 1
}

// redirectLoop is a simple http.HandlerFunc that will redirect to itself.
func redirectLoop(t testing.TB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		t.Logf("trying to redirect to: %s", r.URL)
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}
}

func okHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Helper()
		w.WriteHeader(http.StatusOK)
	}
}

// failHandler is a simple http.HandlerFunc that will always return a 500 (Internal Server Error).
func failHandler(t testing.TB, tries *int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		*tries++
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// timeoutHandler is a simple http.HandlerFunc that will always time out.
func timeoutHandler(t testing.TB) http.HandlerFunc {
	t.Helper()
	return func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(time.Second * 2)
	}
}

// tooManyRequestsHandler is a simple http.HandlerFunc that will always return a 429 (Too Many Requests).
func tooManyRequestsHandler(t *testing.T, tries *int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		*tries++
		http.Error(w, "oh noes", http.StatusTooManyRequests)
	}
}

func TestDefaultResponseChecker(t *testing.T) {
	retryStatuses := []int{
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusHTTPVersionNotSupported,
		http.StatusVariantAlsoNegotiates,
		http.StatusInsufficientStorage,
		http.StatusLoopDetected,
		http.StatusNotExtended,
		http.StatusNetworkAuthenticationRequired,
	}
	tests := []struct {
		headers         map[string]string
		name            string
		statuses        []int
		wantRetryAfter  time.Duration
		wantShouldRetry bool
	}{
		{
			name:            "all 500 and above statuses should retry",
			statuses:        retryStatuses,
			wantShouldRetry: true,
			wantRetryAfter:  0,
		},
		{
			name:            "429 and 503 statuses should honour retry-after",
			headers:         map[string]string{"Retry-After": "10"},
			statuses:        []int{http.StatusTooManyRequests, http.StatusServiceUnavailable},
			wantRetryAfter:  10 * time.Second,
			wantShouldRetry: true,
		},
		{
			name:    "other statuses do not retry",
			headers: map[string]string{"Retry-After": "10"},
			statuses: func() (statuses []int) {
				for i := http.StatusOK; i < http.StatusInternalServerError; i++ {
					if i == http.StatusTooManyRequests {
						continue
					}
					statuses = append(statuses, i)
				}
				return statuses
			}(),
			wantRetryAfter:  0,
			wantShouldRetry: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, code := range tt.statuses {
				gotShouldRetry, gotRetryAfter := httpretry.DefaultResponseChecker(newResponse(t, code, tt.headers))
				if gotShouldRetry != tt.wantShouldRetry {
					t.Errorf("DefaultResponseChecker() gotShouldRetry = %v, want %v", gotShouldRetry, tt.wantShouldRetry)
				}
				if gotRetryAfter != tt.wantRetryAfter {
					t.Errorf("DefaultResponseChecker() gotRetryAfter = %v, want %v", gotRetryAfter, tt.wantRetryAfter)
				}
			}
		})
	}
}

func newResponse(t *testing.T, code int, headers map[string]string) *http.Response {
	response := new(http.Response)
	response.StatusCode = code
	for k, v := range headers {
		response.Header = make(http.Header)
		response.Header.Set(k, v)
	}
	return response
}

func TestPostBodiesArePreserved(t *testing.T) {
	client := httpretry.NewClient()

	// create some non nil request body
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]string{"hello": "world"}); err != nil {
		t.Fatal(err)
	}

	// the post handler will call fatal if the body is empty at any point.
	ts := httptest.NewServer(postHandler(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// post the body to the test handler
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, &buf)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected an error but got none")
	}

	var retryError httpretry.RequestError
	if !errors.As(err, &retryError) {
		t.Fatalf("expected %T, but got: %T", retryError, err)
	}
}

func postHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Helper()

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}

		t.Log(string(raw))

		if string(raw) == "" {
			t.Fatalf("expected request body to be populated")
		}

		w.WriteHeader(http.StatusInternalServerError)
	}
}
