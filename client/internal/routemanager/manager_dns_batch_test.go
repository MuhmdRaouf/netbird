package routemanager

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nbdns "github.com/netbirdio/netbird/client/internal/dns"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/routemanager/client"
	"github.com/netbirdio/netbird/client/internal/routemanager/refcounter"
	"github.com/netbirdio/netbird/client/proto"
	"github.com/netbirdio/netbird/route"
	"github.com/netbirdio/netbird/shared/management/domain"
)

var (
	lanOverlap = netip.MustParsePrefix("172.16.0.0/16")
	reachable  = netip.MustParsePrefix("10.0.0.0/8")
)

// TestUpdateSystemRoutesAppliesDNSBatch checks that DNS route domains reach the host
// configuration even when another route in the same update cannot be installed, e.g. a
// network route that overlaps the local LAN and is rejected by the OS with "file exists".
func TestUpdateSystemRoutesAppliesDNSBatch(t *testing.T) {
	tests := []struct {
		name         string
		staticPrefix netip.Prefix
		wantErr      bool
	}{
		{name: "all routes added", staticPrefix: reachable},
		{name: "static route fails to add", staticPrefix: lanOverlap, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dnsServer := &recordingDNSServer{}
			m := newRouteTestManager(dnsServer.mock(), nil, &routeFailures{add: []netip.Prefix{lanOverlap}})
			staticRoute := staticTestRoute("aws", tt.staticPrefix)
			dnsRoute := dnsTestRoute("app", "app.example.com")

			err := m.updateSystemRoutes(haMap(staticRoute, dnsRoute))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, 1, dnsServer.endBatchCalls, "DNS batch should be applied exactly once")
			assert.Equal(t, domain.List{"app.example.com"}, dnsServer.registered, "DNS route domains should be registered")
			assert.Contains(t, m.activeRoutes, dnsRoute.GetHAUniqueID(), "DNS route should be active")
			if tt.wantErr {
				assert.NotContains(t, m.activeRoutes, staticRoute.GetHAUniqueID(), "failed route should not be active")
			} else {
				assert.Contains(t, m.activeRoutes, staticRoute.GetHAUniqueID(), "static route should be active")
			}
		})
	}
}

// TestUpdateSystemRoutesAppliesDNSBatchOnFailedRemoval checks that a route that fails to be
// removed does not keep the DNS changes of the same update, here the removal of a DNS
// route, from the host configuration.
func TestUpdateSystemRoutesAppliesDNSBatchOnFailedRemoval(t *testing.T) {
	dnsServer := &recordingDNSServer{}
	failures := &routeFailures{}
	m := newRouteTestManager(dnsServer.mock(), peer.NewRecorder("https://mgmt.example.com"), failures)
	staticRoute := staticTestRoute("aws", reachable)
	dnsRoute := dnsTestRoute("app", "app.example.com")
	require.NoError(t, m.updateSystemRoutes(haMap(staticRoute, dnsRoute)))

	// Both routes are withdrawn, and the OS refuses to remove the static one.
	failures.remove = []netip.Prefix{reachable}
	require.Error(t, m.updateSystemRoutes(route.HAMap{}))

	assert.Equal(t, 2, dnsServer.endBatchCalls, "DNS batch should be applied on every update")
	assert.Equal(t, domain.List{"app.example.com"}, dnsServer.deregistered, "removed DNS route should be deregistered")
	assert.Empty(t, m.activeRoutes, "withdrawn routes should not stay active")
}

// TestUpdateSystemRoutesReportsFailedRoutes checks that routes the OS rejects are reported
// as a client event naming the networks, once per distinct set of failures, since failed
// routes are retried on every network map update.
func TestUpdateSystemRoutesReportsFailedRoutes(t *testing.T) {
	otherOverlap := netip.MustParsePrefix("10.1.0.0/16")
	recorder := peer.NewRecorder("https://mgmt.example.com")
	m := newRouteTestManager(&nbdns.MockServer{}, recorder, &routeFailures{add: []netip.Prefix{lanOverlap, otherOverlap}})

	failing := staticTestRoute("aws", lanOverlap)
	otherFailing := staticTestRoute("office", otherOverlap)
	working := staticTestRoute("vpc", reachable)

	// First failure is reported, the retry on the next update is not.
	require.Error(t, m.updateSystemRoutes(haMap(failing, working)))
	require.Error(t, m.updateSystemRoutes(haMap(failing, working)))
	events := failedRouteEvents(recorder)
	require.Len(t, events, 1, "failure should be reported once")
	assert.Equal(t, proto.SystemEvent_WARNING, events[0].Severity, "severity")
	assert.Equal(t, proto.SystemEvent_NETWORK, events[0].Category, "category")
	assert.Equal(t, "172.16.0.0/16", events[0].Metadata["networks"], "event should name the failed network")
	assert.Equal(t,
		"Could not add routes for 172.16.0.0/16. Check for conflicts with your network.",
		events[0].UserMessage, "user message")

	// A second failing route changes the set, so both are reported in a stable order.
	require.Error(t, m.updateSystemRoutes(haMap(failing, otherFailing, working)))
	events = failedRouteEvents(recorder)
	require.Len(t, events, 2, "a changed set of failures should be reported")
	assert.Equal(t, "10.1.0.0/16, 172.16.0.0/16", events[1].Metadata["networks"], "event should name every failed network")

	// Once the routes are gone nothing is reported, and a new failure is reported again.
	require.NoError(t, m.updateSystemRoutes(haMap(working)))
	assert.Len(t, failedRouteEvents(recorder), 2, "recovery should not publish an event")
	require.Error(t, m.updateSystemRoutes(haMap(failing, working)))
	assert.Len(t, failedRouteEvents(recorder), 3, "a new failure should be reported")
}

// routeFailures lists the prefixes the OS refuses to add or remove, the way it rejects a
// route that overlaps a directly connected network.
type routeFailures struct {
	add    []netip.Prefix
	remove []netip.Prefix
}

func newRouteTestManager(dnsServer nbdns.Server, recorder *peer.Status, failures *routeFailures) *DefaultManager {
	return &DefaultManager{
		ctx:            context.Background(),
		dnsServer:      dnsServer,
		statusRecorder: recorder,
		useNewDNSRoute: true,
		activeRoutes:   make(map[route.HAUniqueID]client.RouteHandler),
		routeRefCounter: refcounter.New(
			func(prefix netip.Prefix, _ struct{}) (struct{}, error) {
				if slices.Contains(failures.add, prefix) {
					return struct{}{}, errors.New("add route: file exists")
				}
				return struct{}{}, nil
			},
			func(prefix netip.Prefix, _ struct{}) error {
				if slices.Contains(failures.remove, prefix) {
					return errors.New("remove route: no such process")
				}
				return nil
			},
		),
	}
}

// recordingDNSServer records the handler registrations and batch applications the route
// manager makes against the DNS server.
type recordingDNSServer struct {
	registered    domain.List
	deregistered  domain.List
	endBatchCalls int
}

func (r *recordingDNSServer) mock() *nbdns.MockServer {
	return &nbdns.MockServer{
		RegisterHandlerFunc: func(domains domain.List, _ dns.Handler, _ int) {
			r.registered = append(r.registered, domains...)
		},
		DeregisterHandlerFunc: func(domains domain.List, _ int) {
			r.deregistered = append(r.deregistered, domains...)
		},
		EndBatchFunc: func() { r.endBatchCalls++ },
	}
}

func staticTestRoute(netID string, prefix netip.Prefix) *route.Route {
	return &route.Route{NetID: route.NetID(netID), Network: prefix, NetworkType: route.IPv4Network}
}

func dnsTestRoute(netID string, domains ...domain.Domain) *route.Route {
	return &route.Route{NetID: route.NetID(netID), Domains: domains, NetworkType: route.DomainNetwork}
}

func haMap(routes ...*route.Route) route.HAMap {
	m := make(route.HAMap, len(routes))
	for _, r := range routes {
		m[r.GetHAUniqueID()] = []*route.Route{r}
	}
	return m
}

func failedRouteEvents(recorder *peer.Status) []*proto.SystemEvent {
	var events []*proto.SystemEvent
	for _, e := range recorder.GetEventHistory() {
		if e.Metadata["networks"] != "" {
			events = append(events, e)
		}
	}
	return events
}
