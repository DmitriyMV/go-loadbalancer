// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package controlplane wraps generic TCP loadbalancer for Kubernetes controlplane endpoint LB.
package controlplane

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/siderolabs/go-loadbalancer/loadbalancer"
	"github.com/siderolabs/go-loadbalancer/upstream"
)

// LoadBalancer provides Kubernetes control plane TCP loadbalancer with a way to update endpoints (list of control plane nodes).
type LoadBalancer struct { //nolint:govet
	lb                 loadbalancer.TCP
	healthCheckOptions []upstream.ListOption

	done chan struct{}

	endpoint string
}

// LoadBalancerOption configures the load balancer settings.
type LoadBalancerOption func(*LoadBalancer)

// WithDialTimeout configures the dial timeout.
func WithDialTimeout(timeout time.Duration) LoadBalancerOption {
	return func(lb *LoadBalancer) {
		lb.lb.DialTimeout = timeout
	}
}

// WithKeepAlivePeriod configures the keepalive period.
func WithKeepAlivePeriod(period time.Duration) LoadBalancerOption {
	return func(lb *LoadBalancer) {
		lb.lb.KeepAlivePeriod = period
	}
}

// WithTCPUserTimeout configures the TCP user timeout.
func WithTCPUserTimeout(timeout time.Duration) LoadBalancerOption {
	return func(lb *LoadBalancer) {
		lb.lb.TCPUserTimeout = timeout
	}
}

// WithHealthCheckOptions configures the health check options.
func WithHealthCheckOptions(options ...upstream.ListOption) LoadBalancerOption {
	return func(lb *LoadBalancer) {
		lb.healthCheckOptions = append(lb.healthCheckOptions, options...)
	}
}

// NewLoadBalancer initializes the load balancer.
//
// If bindPort is zero, load balancer will bind to a random available port.
func NewLoadBalancer(bindAddress string, bindPort int, logger *zap.Logger, options ...LoadBalancerOption) (*LoadBalancer, error) {
	if bindPort == 0 {
		var err error

		bindPort, err = findListenPort(bindAddress)
		if err != nil {
			return nil, fmt.Errorf("unable to find available port: %w", err)
		}
	}

	lb := &LoadBalancer{
		endpoint: net.JoinHostPort(bindAddress, strconv.Itoa(bindPort)),
	}

	// set aggressive timeouts to prevent proxying to unhealthy upstreams
	lb.lb.DialTimeout = 5 * time.Second
	lb.lb.KeepAlivePeriod = time.Second
	lb.lb.TCPUserTimeout = 5 * time.Second

	for _, option := range options {
		option(lb)
	}

	if logger == nil {
		logger = zap.Must(zap.NewProduction())
	}

	lb.lb.Logger = logger.Named("controlplane-lb").With(zap.String("endpoint", lb.endpoint))

	// create a route without any upstreams yet
	if err := lb.lb.AddRoute(lb.endpoint, nil, lb.healthCheckOptions...); err != nil {
		return nil, err
	}

	return lb, nil
}

// Endpoint returns loadbalancer endpoint as "host:port".
func (lb *LoadBalancer) Endpoint() string {
	return lb.endpoint
}

// Start the loadbalancer providing a channel which provides endpoint list update.
//
// Load balancer starts with an empty list of endpoints, so initial list should be provided on the channel.
func (lb *LoadBalancer) Start(upstreamCh <-chan []string) error {
	if err := lb.lb.Start(); err != nil {
		return err
	}

	lb.done = make(chan struct{})

	go func() {
		for {
			select {
			case upstreams := <-upstreamCh:
				if err := lb.lb.ReconcileRoute(lb.endpoint, slices.Values(upstreams)); err != nil {
					lb.lb.Logger.Warn("failed reconciling list of upstreams",
						zap.Strings("upstreams", upstreams),
						zap.Error(err),
					)
				}
			case <-lb.done:
				return
			}
		}
	}()

	return nil
}

// Healthy returns true if at least one upstream is available.
func (lb *LoadBalancer) Healthy() (bool, error) {
	return lb.lb.IsRouteHealthy(lb.endpoint)
}

// Shutdown the loadbalancer listener and wait for the connections to be closed.
func (lb *LoadBalancer) Shutdown() error {
	if err := lb.lb.Close(); err != nil {
		return err
	}

	close(lb.done)

	lb.lb.Wait() //nolint:errcheck

	return nil
}

func findListenPort(address string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port //nolint:errcheck,forcetypeassert

	return port, l.Close()
}
