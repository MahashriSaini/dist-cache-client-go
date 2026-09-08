// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build unittest

package dcache

import (
	"context"
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
	r := dnsResolver("10.0.0.10")
	require.NotNil(t, r)
	assert.True(t, r.PreferGo)
	assert.NotNil(t, r.Dial)
}

func TestDNSResolverAcceptsHostPort(t *testing.T) {
	r := dnsResolver("10.0.0.10:5353")
	require.NotNil(t, r)
	assert.True(t, r.PreferGo)
}

func TestWithDNSServerOverridesDefault(t *testing.T) {
	cfg := defaultConfig()

	WithDNSServer("192.0.2.53:5353")(cfg)

	assert.Equal(t, "192.0.2.53:5353", cfg.dnsServer)
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
