package dns

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/netbirdio/netbird/client/internal/dns/local"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/statemanager"
	"github.com/netbirdio/netbird/shared/management/domain"
)

// TestBatchModeAppliesHostConfigOnEnd checks what the host resolver receives for handlers
// registered in batch mode: nothing while the batch is open, then a single host config with
// every registered route domain as a match-only domain once the batch ends.
func TestBatchModeAppliesHostConfigOnEnd(t *testing.T) {
	tests := []struct {
		name          string
		register      []domain.List
		deregister    []domain.List
		wantMatchOnly []string
	}{
		{
			name:          "registered domains are published",
			register:      []domain.List{{"app.example.com"}, {"*.dev.example.com"}},
			wantMatchOnly: []string{"app.example.com.", "dev.example.com."},
		},
		{
			name:          "domain deregistered in the same batch is not published",
			register:      []domain.List{{"app.example.com"}, {"old.example.com"}},
			deregister:    []domain.List{{"old.example.com"}},
			wantMatchOnly: []string{"app.example.com."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var applied []HostDNSConfig
			server := newBatchTestServer(&applied)

			server.BeginBatch()
			for _, domains := range tt.register {
				server.RegisterHandler(domains, &MockHandler{}, PriorityDNSRoute)
			}
			for _, domains := range tt.deregister {
				server.DeregisterHandler(domains, PriorityDNSRoute)
			}
			assert.Empty(t, applied, "host config should not be applied while the batch is open")

			server.EndBatch()
			require.Len(t, applied, 1, "host config should be applied once when the batch ends")
			assert.ElementsMatch(t, tt.wantMatchOnly, matchOnlyDomains(applied[0]), "match-only domains in host config")
		})
	}
}

func newBatchTestServer(applied *[]HostDNSConfig) *DefaultServer {
	return &DefaultServer{
		ctx:          context.Background(),
		handlerChain: NewHandlerChain(),
		hostManager: &mockHostConfigurator{
			applyDNSConfigFunc: func(config HostDNSConfig, _ *statemanager.Manager) error {
				*applied = append(*applied, config)
				return nil
			},
			restoreHostDNSFunc:    func() error { return nil },
			supportCustomPortFunc: func() bool { return true },
			stringFunc:            func() string { return "mock" },
		},
		localResolver:  &local.Resolver{},
		service:        &mockService{},
		statusRecorder: peer.NewRecorder("test"),
		extraDomains:   make(map[domain.Domain]int),
	}
}

func matchOnlyDomains(config HostDNSConfig) []string {
	var domains []string
	for _, d := range config.Domains {
		if d.MatchOnly {
			domains = append(domains, d.Domain)
		}
	}
	return domains
}
