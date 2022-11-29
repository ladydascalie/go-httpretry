# httpretry

[![Go](https://github.com/ladydascalie/go-httpretry/actions/workflows/go.yml/badge.svg)](https://github.com/ladydascalie/go-httpretry/actions/workflows/go.yml)

An http client with support for retry policies and functional options.

## Install

```shell
go get github.com/ladydascalie/go-httpretry
```

## Usage

### Use the provided (sane) default

```go
package main

import "github.com/ladydascalie/go-httpretry"

func main() {
        client := httpretry.NewClient()
        
        ctx, cancel := context.WithTimeout(context.Background(), time.Second)
        defer cancel()
        
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.foo.bar", nil)
        // handle your errors!
        
        res, err := client.Do(req)
        // handle your errors!
}
```

### Customise your usage with functional options

```go
package main

import "github.com/ladydascalie/go-httpretry"

func main() {
        client := httpretry.NewClient(
                        httpretry.WithRetryPolicy(&httpretry.RetryPolicy{
                        Retries:     5,
                        WaitTime:    time.Second,
                        BackoffMode: httpretry.BackoffModeExponential,
                        ResponseCheck: func(response *http.Response) (shouldRetry bool, retryAfter time.Duration) {
                                if response.StatusCode == http.StatusServiceUnavailable {
                                        return true, 30 * time.Second
                                }
                                return false, 0
                        },
                }),
        )

        ctx, cancel := context.WithTimeout(context.Background(), time.Second)
        defer cancel()
        
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.foo.bar", nil)
        // handle your errors!
        
        res, err := client.Do(req)
        // handle your errors!

        resp, err := client.Do(req)
}
```
