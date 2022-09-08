// Package httpretry defines a set of tools to allow creating an http client
// which can be tasked with retrying requests based on configurable policies.
package httpretry

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// defaultLogger is the package's default logger.
var defaultLogger = log.New(os.Stderr, "", log.Lshortfile|log.LstdFlags)

// ResponseCheck defines a function allowing to check on the state of an http response.
type ResponseCheck func(response *http.Response) (shouldRetry bool, retryAfter time.Duration)

// DefaultResponseChecker returns a StatusChecker that checks if the status code is within the given range.
// If status code is greater or equal to 500 (Internal Server Error), this function will return false.
// If the status code is explicitly 429 (Too Many Requests), this function will return false.
// Any other status code will return true.
func DefaultResponseChecker(response *http.Response) (shouldRetry bool, retryAfter time.Duration) {
	code := response.StatusCode

	switch {
	// this case comes first to avoid catching 429 in the second case.
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		str := response.Header.Get("Retry-After")

		// test for Retry-After: <delay-seconds>
		if sleep, err := strconv.ParseInt(str, 10, 64); err == nil {
			return true, time.Duration(sleep) * time.Second
		}

		// test for Retry-After: <http-date>
		now := time.Now()
		if date, err := time.Parse(time.RFC1123, str); err == nil && now.Before(date) {
			return true, time.Duration(date.Unix()-now.Unix()) * time.Second
		}

		return true, 0
	case code >= http.StatusInternalServerError:
		return true, 0
	default:
		return false, 0
	}
}

// RetryPolicy is a retry policy used by the RetryTransport.
type RetryPolicy struct {
	ResponseCheck ResponseCheck
	Retries       int
	WaitTime      time.Duration
	BackoffMode   BackoffMode
}

func NewDefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		Retries:       3,
		WaitTime:      time.Second,
		BackoffMode:   BackoffModeExponential,
		ResponseCheck: DefaultResponseChecker,
	}
}

// ClientOption is a function that configures a http.Client.
type ClientOption func(client *http.Client)

// WithTimeout returns a new http.Client with the given timeout.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(client *http.Client) {
		client.Timeout = timeout
	}
}

// WithRetryPolicy returns a new http.Client with the given retry policy.
func WithRetryPolicy(policy *RetryPolicy) ClientOption {
	return func(client *http.Client) {
		client.Transport = NewRetryTransport(policy)
	}
}

// WithCookieJar returns a new http.Client with the given cookie jar.
func WithCookieJar(jar http.CookieJar) ClientOption {
	return func(client *http.Client) {
		client.Jar = jar
	}
}

// WithCheckRedirect returns a new http.Client with the given check redirect func.
func WithCheckRedirect(f func(req *http.Request, via []*http.Request) error) ClientOption {
	return func(client *http.Client) {
		client.CheckRedirect = f
	}
}

// Logger defines any logger able to call Printf.
type Logger interface {
	Printf(format string, v ...interface{})
}

func WithLogger(logger Logger) ClientOption {
	return func(client *http.Client) {
		// While this is a __little__ annoying, it does allow to keep the
		// client setup code simple and readable.
		if transport, ok := client.Transport.(*RetryTransport); ok {
			transport.Logger = logger
		}
	}
}

// NewClient returns a new http.Client with the given options.
func NewClient(options ...ClientOption) *http.Client {
	client := &http.Client{
		Transport: NewRetryTransport(NewDefaultRetryPolicy()),
		Timeout:   10 * time.Second,
	}

	for _, option := range options {
		option(client)
	}

	return client
}

// BackoffMode is the backoff method used by the retry transport.
type BackoffMode int

const (
	BackoffModeLinear BackoffMode = iota
	BackoffModeExponential
)

// RetryTransport is an http.RoundTripper that retries requests.
type RetryTransport struct {
	http.RoundTripper
	Policy *RetryPolicy
	Logger Logger
}

// NewRetryTransport returns a new http.RoundTripper with the given retry policy.
// If no policy is given, the default policy is used.
// default policy: RetryPolicy{Retries: 3, WaitTime: 1 * time.Second, BackoffMode: BackoffModeExponential}
func NewRetryTransport(policy *RetryPolicy) *RetryTransport {
	defaultPolicy := &RetryPolicy{
		Retries:     3,
		WaitTime:    500 * time.Millisecond,
		BackoffMode: BackoffModeExponential,
	}

	// only if no policy is given.
	if policy == nil {
		policy = defaultPolicy
	}

	// setup transport with some defaults
	transport := &RetryTransport{
		RoundTripper: http.DefaultTransport,
		Policy:       policy,
		Logger:       defaultLogger,
	}

	return transport
}

// RequestError is returned when the maximum number of retries is exceeded.
type RequestError struct {
	Err      error
	Response *http.Response
}

func (e RequestError) Unwrap() error { return e.Err }

// Error returns the error message.
func (e RequestError) Error() string {
	const baseError = "max retries exceeded"
	if e.Err == nil {
		if e.Response != nil {
			return fmt.Sprintf("%s: %s", baseError, e.Response.Status)
		}
		return baseError
	}
	return fmt.Sprintf("%s: %v", baseError, e.Err)
}

// RoundTrip implements the http.RoundTripper interface.
func (r *RetryTransport) RoundTrip(req *http.Request) (response *http.Response, err error) {
	for i := 1; i <= r.Policy.Retries; i++ {
		response, err = r.RoundTripper.RoundTrip(req)
		if err != nil {
			r.Logger.Printf("error during request #%d, will retry in %s: %v", i, r.Policy.WaitTime, err)
			backoff(r.Policy, i)
			continue
		}

		if r.Policy.ResponseCheck != nil {
			shouldRetry, retryAfter := r.Policy.ResponseCheck(response)
			if !shouldRetry {
				return response, nil
			}

			policyWait := r.Policy.WaitTime

			// if the retry-after value asked is greater than
			// what our policy configures, use that value instead.
			if retryAfter > policyWait {
				policyWait = retryAfter
			}

			r.Logger.Printf(
				"error during request #%d, will retry in %s: %v", i, policyWait,
				fmt.Errorf("unexpected status code: %s", response.Status),
			)

			// in the case where we need to honour a retry-after header
			// we send in a one-time policy which matches the desired wait time.
			if retryAfter != 0 {
				backoff(&RetryPolicy{
					WaitTime:    retryAfter,
					BackoffMode: BackoffModeLinear,
				}, 1)
				continue
			}

			// all other cases...
			backoff(r.Policy, i)
			continue
		}
	}

	return nil, RequestError{
		Err:      err,
		Response: response,
	}
}

// backoff handles the common switch-case for backoff modes.
func backoff(policy *RetryPolicy, try int) {
	switch policy.BackoffMode {
	case BackoffModeExponential:
		time.Sleep(policy.WaitTime * time.Duration(try))
	case BackoffModeLinear:
		fallthrough
	default:
		time.Sleep(policy.WaitTime)
	}
}
