/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package dockerconfigresolver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

var PushTracker = docker.NewInMemoryTracker()

// Global semaphores per registry host to enforce true concurrency limits
var (
	semaphoresMutex sync.RWMutex
	semaphores      = make(map[string]chan struct{})
)

// getSemaphore returns or creates a semaphore for a given host with the specified limit
func getSemaphore(host string, limit int) chan struct{} {
	semaphoresMutex.Lock()
	defer semaphoresMutex.Unlock()
	
	key := fmt.Sprintf("%s:%d", host, limit)
	if sem, exists := semaphores[key]; exists {
		return sem
	}
	
	// Create a new semaphore with the specified limit
	sem := make(chan struct{}, limit)
	semaphores[key] = sem
	log.L.Debugf("Created semaphore for %s with limit %d", host, limit)
	return sem
}

// semaphoreTransport wraps an http.RoundTripper to enforce true concurrency limits using semaphores
type semaphoreTransport struct {
	transport http.RoundTripper
	limit     int
}

// RoundTrip implements http.RoundTripper with semaphore-based concurrency limiting
func (st *semaphoreTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if st.limit <= 0 {
		// No limit, use underlying transport directly
		return st.transport.RoundTrip(req)
	}
	
	host := req.URL.Host
	sem := getSemaphore(host, st.limit)
	
	// Acquire semaphore (blocks if limit reached)
	log.L.Debugf("Acquiring semaphore for %s (limit %d)", host, st.limit)
	sem <- struct{}{}
	defer func() {
		<-sem // Release semaphore
		log.L.Debugf("Released semaphore for %s", host)
	}()
	
	log.L.Debugf("Acquired semaphore for %s, making request", host)
	return st.transport.RoundTrip(req)
}

// retryTransport wraps an http.RoundTripper to add retry logic for 503 errors
type retryTransport struct {
	transport    http.RoundTripper
	maxRetries   int
	initialDelay time.Duration
}

// RoundTrip implements http.RoundTripper with retry logic for 503 Service Unavailable errors
func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; attempt <= rt.maxRetries; attempt++ {
		// Clone the request for potential retries
		reqClone := req.Clone(req.Context())
		
		resp, err := rt.transport.RoundTrip(reqClone)
		
		// If no error or not a 503, return immediately
		if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
			return resp, err
		}
		
		// If this is the last attempt, return the 503 response
		if attempt == rt.maxRetries {
			log.L.Warnf("Max retries (%d) exceeded for 503 Service Unavailable from %s", rt.maxRetries, req.URL.Host)
			return resp, err
		}
		
		// Close the response body before retrying
		if resp.Body != nil {
			resp.Body.Close()
		}
		
		// Calculate exponential backoff delay: initialDelay * 2^attempt
		delay := time.Duration(float64(rt.initialDelay) * math.Pow(2, float64(attempt)))
		log.L.Infof("503 Service Unavailable from %s, retrying in %v (attempt %d/%d)", 
			req.URL.Host, delay, attempt+1, rt.maxRetries)
		
		// Wait before retrying
		time.Sleep(delay)
	}
	
	// This should never be reached, but return error just in case
	return nil, fmt.Errorf("unexpected retry logic error")
}

type opts struct {
	plainHTTP         bool
	skipVerifyCerts   bool
	hostsDirs         []string
	authCreds         AuthCreds
	maxConnsPerHost   int
	maxIdleConns      int
	requestTimeout    time.Duration
	maxRetries        int
	retryInitialDelay time.Duration
}

// Opt for New
type Opt func(*opts)

// WithPlainHTTP enables insecure plain HTTP
func WithPlainHTTP(b bool) Opt {
	return func(o *opts) {
		o.plainHTTP = b
	}
}

// WithSkipVerifyCerts skips verifying TLS certs
func WithSkipVerifyCerts(b bool) Opt {
	return func(o *opts) {
		o.skipVerifyCerts = b
	}
}

// WithHostsDirs specifies directories like /etc/containerd/certs.d and /etc/docker/certs.d
func WithHostsDirs(orig []string) Opt {
	validDirs := validateDirectories(orig)
	return func(o *opts) {
		o.hostsDirs = validDirs
	}
}

func WithAuthCreds(ac AuthCreds) Opt {
	return func(o *opts) {
		o.authCreds = ac
	}
}

// WithMaxConnsPerHost sets the maximum number of connections per host
func WithMaxConnsPerHost(n int) Opt {
	return func(o *opts) {
		o.maxConnsPerHost = n
	}
}

// WithMaxIdleConns sets the maximum number of idle connections
func WithMaxIdleConns(n int) Opt {
	return func(o *opts) {
		o.maxIdleConns = n
	}
}

// WithRequestTimeout sets the request timeout
func WithRequestTimeout(d time.Duration) Opt {
	return func(o *opts) {
		o.requestTimeout = d
	}
}

// WithMaxRetries sets the maximum number of retry attempts for 503 errors
func WithMaxRetries(n int) Opt {
	return func(o *opts) {
		o.maxRetries = n
	}
}

// WithRetryInitialDelay sets the initial delay before first retry
func WithRetryInitialDelay(d time.Duration) Opt {
	return func(o *opts) {
		o.retryInitialDelay = d
	}
}

// NewHostOptions instantiates a HostOptions struct using $DOCKER_CONFIG/config.json .
//
// $DOCKER_CONFIG defaults to "~/.docker".
//
// refHostname is like "docker.io".
func NewHostOptions(ctx context.Context, refHostname string, optFuncs ...Opt) (*dockerconfig.HostOptions, error) {
	var o opts
	for _, of := range optFuncs {
		of(&o)
	}
	var ho dockerconfig.HostOptions

	ho.HostDir = func(hostURL string) (string, error) {
		regURL, err := Parse(hostURL)
		// Docker inconsistencies handling: `index.docker.io` actually expects `docker.io` for hosts.toml on the filesystem
		// See https://github.com/containerd/nerdctl/issues/3697
		// FIXME: we need to reevaluate this comparing with what docker does. What should happen for FQ images with alternate docker domains? (eg: registry-1.docker.io)
		if regURL.Hostname() == "index.docker.io" {
			regURL.Host = "docker.io"
		}

		if err != nil {
			return "", err
		}
		dir, err := hostDirsFromRoot(regURL, o.hostsDirs)
		if err != nil {
			if errors.Is(err, errdefs.ErrNotFound) {
				err = nil
			}
			return "", err
		}
		return dir, nil
	}

	if o.authCreds != nil {
		ho.Credentials = o.authCreds
	} else {
		authCreds, err := NewAuthCreds(refHostname)
		if err != nil {
			return nil, err
		}
		ho.Credentials = authCreds

	}

	if o.skipVerifyCerts {
		ho.DefaultTLS = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	if o.plainHTTP {
		ho.DefaultScheme = "http"
	} else {
		if isLocalHost, err := docker.MatchLocalhost(refHostname); err != nil {
			return nil, err
		} else if isLocalHost {
			ho.DefaultScheme = "http"
		}
	}
	if ho.DefaultScheme == "http" {
		// https://github.com/containerd/containerd/issues/9208
		ho.DefaultTLS = nil
	}
	return &ho, nil
}

// New instantiates a resolver using $DOCKER_CONFIG/config.json .
//
// $DOCKER_CONFIG defaults to "~/.docker".
//
// refHostname is like "docker.io".
func New(ctx context.Context, refHostname string, optFuncs ...Opt) (remotes.Resolver, error) {
	ho, err := NewHostOptions(ctx, refHostname, optFuncs...)
	if err != nil {
		return nil, err
	}

	resolverOpts := docker.ResolverOptions{
		Tracker: PushTracker,
		Hosts:   dockerconfig.ConfigureHosts(ctx, *ho),
	}

	// Configure HTTP client with connection limits to prevent registry overload
	var o opts
	for _, of := range optFuncs {
		of(&o)
	}

	// Always apply custom HTTP client when any connection limits or retry options are specified
	// This ensures user-specified limits take effect, including restrictive values like 1
	if o.maxConnsPerHost > 0 || o.maxIdleConns > 0 || o.requestTimeout > 0 || o.maxRetries > 0 {
		log.L.Debugf("Applying custom HTTP client with limits: maxConnsPerHost=%d, maxIdleConns=%d, requestTimeout=%v, maxRetries=%d", 
			o.maxConnsPerHost, o.maxIdleConns, o.requestTimeout, o.maxRetries)
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}

		// Apply connection limits - use exact values when specified, defaults otherwise
		if o.maxIdleConns > 0 {
			transport.MaxIdleConns = o.maxIdleConns
		} else {
			transport.MaxIdleConns = 100 // Default
		}

		if o.maxConnsPerHost > 0 {
			transport.MaxConnsPerHost = o.maxConnsPerHost
			// Also set MaxIdleConnsPerHost to ensure per-host limits are respected
			transport.MaxIdleConnsPerHost = o.maxConnsPerHost
			
			// For very restrictive limits (1-2 connections), disable keep-alives to ensure
			// strict connection limiting and prevent connection reuse issues
			if o.maxConnsPerHost <= 2 {
				transport.DisableKeepAlives = true
				log.L.Debugf("Disabled keep-alives for strict connection limit of %d", o.maxConnsPerHost)
			}
			
			log.L.Debugf("Set MaxConnsPerHost=%d and MaxIdleConnsPerHost=%d for registry %s", 
				o.maxConnsPerHost, o.maxConnsPerHost, refHostname)
		} else {
			transport.MaxConnsPerHost = 0 // Use Go's default (no limit)
		}

		// Wrap transport with semaphore-based concurrency limiting
		var finalTransport http.RoundTripper = transport
		if o.maxConnsPerHost > 0 {
			finalTransport = &semaphoreTransport{
				transport: finalTransport,
				limit:     o.maxConnsPerHost,
			}
			log.L.Debugf("Enabled semaphore-based concurrency limiting: limit=%d", o.maxConnsPerHost)
		}

		// Wrap with retry logic if retries are configured
		if o.maxRetries > 0 {
			retryDelay := o.retryInitialDelay
			if retryDelay == 0 {
				retryDelay = 1000 * time.Millisecond // Default to 1 second
			}
			finalTransport = &retryTransport{
				transport:    finalTransport,
				maxRetries:   o.maxRetries,
				initialDelay: retryDelay,
			}
			log.L.Debugf("Enabled retry logic: maxRetries=%d, initialDelay=%v", o.maxRetries, retryDelay)
		}

		client := &http.Client{
			Transport: finalTransport,
		}

		if o.requestTimeout > 0 {
			client.Timeout = o.requestTimeout
		}

		resolverOpts.Client = client
	}

	resolver := docker.NewResolver(resolverOpts)
	return resolver, nil
}

// AuthCreds is for docker.WithAuthCreds
type AuthCreds func(string) (string, string, error)

// NewAuthCreds returns AuthCreds that uses $DOCKER_CONFIG/config.json .
// AuthCreds can be nil.
func NewAuthCreds(refHostname string) (AuthCreds, error) {
	// Note: does not raise an error on ENOENT
	credStore, err := NewCredentialsStore("")
	if err != nil {
		return nil, err
	}

	credFunc := func(host string) (string, string, error) {
		rHost, err := Parse(host)
		if err != nil {
			return "", "", err
		}

		ac, err := credStore.Retrieve(rHost, true)
		if err != nil {
			return "", "", err
		}

		if ac.IdentityToken != "" {
			return "", ac.IdentityToken, nil
		}

		if ac.RegistryToken != "" {
			// Even containerd/CRI does not support RegistryToken as of v1.4.3,
			// so, nobody is actually using RegistryToken?
			log.L.Warnf("ac.RegistryToken (for %q) is not supported yet (FIXME)", rHost.Host)
		}

		return ac.Username, ac.Password, nil
	}

	return credFunc, nil
}

