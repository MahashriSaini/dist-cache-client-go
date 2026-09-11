// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build unittest

package dcache

import (
	"bytes"
	"context"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServersFromSRVUsesAdvertisedPorts(t *testing.T) {
	addrs := []*net.SRV{
		{Target: "cache-1.cache.svc.cluster.local.", Port: 19065},
		{Target: "cache-0.cache.svc.cluster.local.", Port: 29065},
	}

	assert.Equal(t, []string{
		"cache-0.cache.svc.cluster.local:29065",
		"cache-1.cache.svc.cluster.local:19065",
	}, serversFromSRV(addrs))
}

func TestDNSResolverEmptyServerReturnsNil(t *testing.T) {
	assert.Nil(t, dnsResolver(""))
}

func TestDNSResolverNonEmptyServerReturnsResolver(t *testing.T) {
	r := dnsResolver("192.0.2.53")
	require.NotNil(t, r)
	assert.True(t, r.PreferGo)
	assert.NotNil(t, r.Dial)
}

func TestDNSResolverAcceptsHostPort(t *testing.T) {
	r := dnsResolver("192.0.2.53:5353")
	require.NotNil(t, r)
	assert.True(t, r.PreferGo)
}

func TestWithDNSServerOverridesDefault(t *testing.T) {
	cfg := defaultConfig()
	assert.Empty(t, cfg.dnsServer)

	WithDNSServer("192.0.2.53:5353")(cfg)

	assert.Equal(t, "192.0.2.53:5353", cfg.dnsServer)
}

func TestDNSServerNameUsesDefaultPort(t *testing.T) {
	assert.Equal(t, "192.0.2.53:53", dnsServerName("192.0.2.53"))
	assert.Equal(t, "192.0.2.53:5353", dnsServerName("192.0.2.53:5353"))
	assert.Equal(t, "system", dnsServerName(""))
}

func TestConnPoolDialHonorsCanceledContext(t *testing.T) {
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(originalOutput)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pool := newConnPool(
		"cache.example.invalid:9065",
		1,
		time.Minute,
		0,
		dnsResolver("192.0.2.53"),
		"192.0.2.53:53",
	)

	_, err := pool.dial(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, ErrConnectionFailed)
	assert.Contains(t, logs.String(), `cache server address resolution failed host="cache.example.invalid"`)
	assert.Contains(t, logs.String(), `dns_server="192.0.2.53:53"`)
}

func TestConnPoolDialLogsResolvedRemoteAddress(t *testing.T) {
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(originalOutput)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		nc, err := listener.Accept()
		if err == nil {
			nc.Close()
		}
		close(accepted)
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	pool := newConnPool(net.JoinHostPort("localhost", port), 1, time.Second, 0, nil, "system")

	clientConn, err := pool.dial(context.Background())
	require.NoError(t, err)
	clientConn.close()
	<-accepted

	assert.Contains(t, logs.String(), `cache server address resolved host="localhost"`)
	assert.Contains(t, logs.String(), `remote_address="127.0.0.1:`)
	assert.Contains(t, logs.String(), `dns_server="system"`)
}

func TestConnPoolDialAllowsZeroTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		nc, err := listener.Accept()
		if err == nil {
			nc.Close()
		}
		close(accepted)
	}()

	pool := newConnPool(listener.Addr().String(), 1, 0, 0, nil, "system")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	clientConn, err := pool.dial(ctx)
	require.NoError(t, err)
	clientConn.close()
	<-accepted
}

func TestK8sDNSDiscoveryHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := defaultConfig()
	cfg.dnsServer = "192.0.2.53"
	manager := newConnManager(1, time.Minute, 0, dnsResolver(cfg.dnsServer), dnsServerName(cfg.dnsServer))
	d := &discovery{cfg: cfg, connMgr: manager}

	_, err := d.discoverViaK8sDNS(ctx, "cache", "default", defaultPort)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestDNSResolverRoutesQueriesToConfiguredServer verifies that queries made through
// the returned resolver are sent to the configured endpoint and not to whatever
// /etc/resolv.conf specifies.
func TestDNSResolverRoutesQueriesToConfiguredServer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()

	type captured struct {
		local  string // address the packet was received on
		remote string // source address of the resolver client
	}
	got := make(chan captured, 4)
	go func() {
		buf := make([]byte, 512)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case got <- captured{local: pc.LocalAddr().String(), remote: addr.String()}:
			default:
			}
		}
	}()

	endpoint := pc.LocalAddr().String()
	r := dnsResolver(endpoint)
	require.NotNil(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The fake server never replies, so the lookup will fail; we only care
	// that a packet reached our chosen endpoint.
	_, _ = r.LookupHost(ctx, "example.invalid.")

	select {
	case c := <-got:
		assert.Equal(t, endpoint, c.local,
			"DNS packet did not land on the configured resolver endpoint")
		assert.True(t, strings.HasPrefix(c.remote, "127.0.0.1:"),
			"query came from an unexpected source: %s", c.remote)
		t.Logf("dnsResolver query landed on %s from %s", c.local, c.remote)
	case <-time.After(2 * time.Second):
		t.Fatal("custom DNS server did not receive a query")
	}
}
