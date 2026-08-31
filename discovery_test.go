// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build unittest

package dcache

import (
	"context"
	"net"
	"strings"
	"sync"
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

// TestDNSResolverRoutesQueriesToConfiguredServer verifies that queries made through
// the returned resolver are sent to the configured endpoint and not to whatever
// /etc/resolv.conf specifies. This is the T1 POC assertion: DNS packets emitted
// by dnsResolver() must land on the configured server address.
func TestDNSResolverRoutesQueriesToConfiguredServer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()

	type captured struct {
		local  string // address the packet was received on (our fake server)
		remote string // source address of the client that sent it
		qname  string
		qtype  uint16
	}
	got := make(chan captured, 4)
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			qname, qtype, ok := parseDNSQuestion(buf[:n])
			if !ok {
				continue
			}
			select {
			case got <- captured{
				local:  pc.LocalAddr().String(),
				remote: addr.String(),
				qname:  qname,
				qtype:  qtype,
			}:
			default:
			}
		}
	}()

	endpoint := pc.LocalAddr().String()
	r := dnsResolver(endpoint)
	require.NotNil(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The fake server never replies, so the lookup itself will fail; we only care
	// that a packet reached our chosen endpoint carrying the expected question.
	_, _ = r.LookupHost(ctx, "example.invalid.")

	select {
	case c := <-got:
		assert.Equal(t, endpoint, c.local,
			"DNS packet did not land on the configured resolver endpoint")
		assert.True(t, strings.HasPrefix(c.remote, "127.0.0.1:"),
			"query came from an unexpected source: %s", c.remote)
		assert.Equal(t, "example.invalid.", c.qname,
			"resolver did not send the expected question name")
		t.Logf("dnsResolver query landed on %s from %s: qname=%s qtype=%d",
			c.local, c.remote, c.qname, c.qtype)
	case <-time.After(2 * time.Second):
		t.Fatal("custom DNS server did not receive a query")
	}
}

// TestDiscoverViaK8sDNSUsesCustomResolver is the T2 POC assertion: the discovery
// layer must route its SRV lookup for the k8s headless service through
// dnsResolver(cfg.dnsServer), never the system resolver.
func TestDiscoverViaK8sDNSUsesCustomResolver(t *testing.T) {
	const (
		svc = "cacheserver"
		ns  = "default"
		prt = 9065
	)
	wantQName := svc + "." + ns + ".svc.cluster.local."

	stub, queries, stop := startFakeDNSServer(t, []dnsSRVAnswer{
		{priority: 10, weight: 10, port: prt, target: "cacheserver-0.cacheserver.default.svc.cluster.local."},
		{priority: 10, weight: 10, port: prt, target: "cacheserver-1.cacheserver.default.svc.cluster.local."},
	})
	defer stop()

	d := &discovery{cfg: &clientConfig{
		dnsServer:    stub,
		k8sService:   svc,
		k8sNamespace: ns,
		port:         prt,
	}}

	servers, err := d.discoverViaK8sDNS(svc, ns, prt)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"cacheserver-0.cacheserver.default.svc.cluster.local:9065",
		"cacheserver-1.cacheserver.default.svc.cluster.local:9065",
	}, servers)

	// Assert the SRV question was actually sent to our stub, not elsewhere.
	sawSRV := false
	for _, q := range queries.snapshot() {
		if strings.EqualFold(q.name, wantQName) && q.qtype == dnsTypeSRV {
			sawSRV = true
			break
		}
	}
	assert.True(t, sawSRV,
		"expected SRV query for %s at %s; got %+v", wantQName, stub, queries.snapshot())
	t.Logf("discoverViaK8sDNS sent %d queries to stub %s: %+v",
		len(queries.snapshot()), stub, queries.snapshot())
}

// ---------------------------------------------------------------------------
// Minimal DNS stub for unit tests. Handles only what the POC needs:
//   * parses a single-question query
//   * answers SRV queries with the canned answers
//   * ignores everything else (client falls back / errors out)
// ---------------------------------------------------------------------------

const (
	dnsTypeA    uint16 = 1
	dnsTypeAAAA uint16 = 28
	dnsTypeSRV  uint16 = 33
)

type dnsSRVAnswer struct {
	priority, weight, port uint16
	target                 string
}

type dnsQuery struct {
	name  string
	qtype uint16
}

type dnsQueryLog struct {
	mu sync.Mutex
	qs []dnsQuery
}

func (l *dnsQueryLog) add(q dnsQuery) {
	l.mu.Lock()
	l.qs = append(l.qs, q)
	l.mu.Unlock()
}

func (l *dnsQueryLog) snapshot() []dnsQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]dnsQuery, len(l.qs))
	copy(out, l.qs)
	return out
}

// startFakeDNSServer starts a UDP DNS responder on 127.0.0.1 that answers SRV
// queries with the given answers. Returns the listen address, a running query
// log, and a stop function.
func startFakeDNSServer(t *testing.T, srv []dnsSRVAnswer) (string, *dnsQueryLog, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)

	log := &dnsQueryLog{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			qname, qtype, ok := parseDNSQuestion(q)
			if !ok {
				continue
			}
			log.add(dnsQuery{name: qname, qtype: qtype})
			if qtype == dnsTypeSRV {
				_, _ = pc.WriteTo(buildSRVResponse(q, srv), addr)
			}
			// For A/AAAA we intentionally stay silent; the client will
			// give up on that record type quickly enough for the POC.
		}
	}()

	stop := func() {
		_ = pc.Close()
		<-done
	}
	return pc.LocalAddr().String(), log, stop
}

// parseDNSQuestion extracts the first question's QNAME (lower-case, trailing dot)
// and QTYPE from a DNS message. It handles only uncompressed labels, which is
// what stdlib clients send in the question section.
func parseDNSQuestion(msg []byte) (string, uint16, bool) {
	if len(msg) < 12 {
		return "", 0, false
	}
	off := 12
	var labels []string
	for off < len(msg) {
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l&0xc0 != 0 || off+l > len(msg) {
			return "", 0, false
		}
		labels = append(labels, string(msg[off:off+l]))
		off += l
	}
	if off+4 > len(msg) {
		return "", 0, false
	}
	qtype := uint16(msg[off])<<8 | uint16(msg[off+1])
	return strings.ToLower(strings.Join(labels, ".")) + ".", qtype, true
}

// buildSRVResponse constructs a DNS response for a SRV query. It echoes the
// original query's header ID and question section, then appends the given SRV
// answers.
func buildSRVResponse(query []byte, answers []dnsSRVAnswer) []byte {
	// Find end of question section in the original query.
	off := 12
	for off < len(query) {
		l := int(query[off])
		off++
		if l == 0 {
			break
		}
		off += l
	}
	off += 4 // qtype + qclass
	if off > len(query) {
		return nil
	}

	resp := make([]byte, 0, 512)
	resp = append(resp, query[0], query[1])   // ID
	resp = append(resp, 0x81, 0x80)           // Flags: QR=1, RD=1, RA=1
	resp = append(resp, 0x00, 0x01)           // QDCOUNT=1
	ancount := uint16(len(answers))
	resp = append(resp, byte(ancount>>8), byte(ancount)) // ANCOUNT
	resp = append(resp, 0x00, 0x00, 0x00, 0x00)          // NSCOUNT, ARCOUNT

	// Echo question section verbatim.
	resp = append(resp, query[12:off]...)

	for _, a := range answers {
		resp = append(resp, 0xc0, 0x0c)             // NAME: pointer to QNAME
		resp = append(resp, 0x00, 0x21)             // TYPE=SRV
		resp = append(resp, 0x00, 0x01)             // CLASS=IN
		resp = append(resp, 0x00, 0x00, 0x00, 0x1e) // TTL=30
		rd := make([]byte, 0, 32)
		rd = append(rd, byte(a.priority>>8), byte(a.priority))
		rd = append(rd, byte(a.weight>>8), byte(a.weight))
		rd = append(rd, byte(a.port>>8), byte(a.port))
		rd = append(rd, encodeDomainName(a.target)...)
		resp = append(resp, byte(len(rd)>>8), byte(len(rd)))
		resp = append(resp, rd...)
	}
	return resp
}

func encodeDomainName(name string) []byte {
	name = strings.TrimSuffix(name, ".")
	var out []byte
	if name != "" {
		for _, label := range strings.Split(name, ".") {
			out = append(out, byte(len(label)))
			out = append(out, []byte(label)...)
		}
	}
	out = append(out, 0)
	return out
}
