package netproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type TransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
}

var transportConfig atomic.Value

const defaultMaxIdleConns = 20000
const defaultMaxIdleConnsPerHost = 2048

func ConfigureTransport(config TransportConfig) {
	transportConfig.Store(normalizeTransportConfig(config))
}

func NewTransport(responseHeaderTimeout time.Duration) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	var transport *http.Transport
	if ok {
		transport = base.Clone()
	} else {
		transport = defaultTransport()
	}
	config := currentTransportConfig()
	transport.Proxy = nil
	transport.DialContext = directDialContext()
	transport.MaxIdleConns = config.MaxIdleConns
	transport.MaxIdleConnsPerHost = config.MaxIdleConnsPerHost
	transport.MaxConnsPerHost = config.MaxConnsPerHost
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = time.Second
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return transport
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		DialContext:           directDialContext(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func currentTransportConfig() TransportConfig {
	value := transportConfig.Load()
	if value == nil {
		return normalizeTransportConfig(TransportConfig{})
	}
	config, ok := value.(TransportConfig)
	if !ok {
		return normalizeTransportConfig(TransportConfig{})
	}
	return normalizeTransportConfig(config)
}

func normalizeTransportConfig(config TransportConfig) TransportConfig {
	if config.MaxIdleConns <= 0 {
		config.MaxIdleConns = defaultMaxIdleConns
	}
	if config.MaxIdleConnsPerHost <= 0 {
		config.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}
	if config.MaxConnsPerHost < 0 {
		config.MaxConnsPerHost = 0
	}
	return config
}

func NewHTTPClient(proxyURL string, timeout time.Duration, responseHeaderTimeout time.Duration) (*http.Client, error) {
	transport := NewTransport(responseHeaderTimeout)
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return &http.Client{Transport: transport, Timeout: timeout}, nil
	}
	if err := ConfigureTransportProxy(transport, proxyURL); err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func ConfigureTransportProxy(transport *http.Transport, proxyURL string) error {
	if transport == nil {
		return fmt.Errorf("transport is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return err
	}
	transport.Proxy = nil
	transport.DialContext = directDialContext()
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		dialer, err := socks5Dialer(parsed)
		if err != nil {
			return err
		}
		if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
			transport.DialContext = contextDialer.DialContext
		} else {
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				type result struct {
					conn net.Conn
					err  error
				}
				done := make(chan result, 1)
				go func() {
					conn, err := dialer.Dial(network, address)
					done <- result{conn: conn, err: err}
				}()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case result := <-done:
					return result.conn, result.err
				}
			}
		}
	default:
		return fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	return nil
}

func directDialContext() func(context.Context, string, string) (net.Conn, error) {
	return (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
}

func socks5Dialer(parsed *url.URL) (xproxy.Dialer, error) {
	var auth *xproxy.Auth
	if parsed.User != nil {
		auth = &xproxy.Auth{User: parsed.User.Username()}
		if password, ok := parsed.User.Password(); ok {
			auth.Password = password
		}
	}
	return xproxy.SOCKS5("tcp", parsed.Host, auth, xproxy.Direct)
}
