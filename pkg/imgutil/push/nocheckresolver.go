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

package push

import (
	"fmt"
	"net/http"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
)

// noCheckTransport wraps an HTTP transport and skips HEAD requests to avoid rate limiting
type noCheckTransport struct {
	base http.RoundTripper
}

func (t *noCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Skip ALL HEAD requests entirely to avoid rate limiting
	// Return 404 immediately without contacting the registry
	if req.Method == http.MethodHead {
		fmt.Printf("nerdctl: skipping HEAD request to %s (--skip-existing-layers-check)\n", req.URL.String())
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	
	// For all other requests, use the original transport
	return t.base.RoundTrip(req)
}

// NewNoCheckResolver creates a resolver that skips HEAD requests for layer existence checks
// This avoids rate limiting issues by not sending HEAD requests to the registry at all
func NewNoCheckResolver(resolverOpts docker.ResolverOptions) remotes.Resolver {
	// Wrap the Hosts function to provide custom HTTP client
	originalHosts := resolverOpts.Hosts
	resolverOpts.Hosts = func(hostname string) ([]docker.RegistryHost, error) {
		hosts, err := originalHosts(hostname)
		if err != nil {
			return nil, err
		}
		
		// Apply custom HTTP client to all hosts
		for i := range hosts {
			if hosts[i].Client == nil {
				hosts[i].Client = http.DefaultClient
			}
			// Create a new client with wrapped transport
			originalClient := hosts[i].Client
			wrappedTransport := &noCheckTransport{
				base: originalClient.Transport,
			}
			if wrappedTransport.base == nil {
				wrappedTransport.base = http.DefaultTransport
			}
			
			// Create new client with wrapped transport
			hosts[i].Client = &http.Client{
				Transport:     wrappedTransport,
				CheckRedirect: originalClient.CheckRedirect,
				Jar:           originalClient.Jar,
				Timeout:       originalClient.Timeout,
			}
		}
		
		return hosts, nil
	}
	
	return docker.NewResolver(resolverOpts)
}