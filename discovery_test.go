// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build unittest

package dcache

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
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
