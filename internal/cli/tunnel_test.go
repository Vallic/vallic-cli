package cli

import (
	"context"
	"strings"
	"testing"
)

// A tunnel names a service, and only one the machine can reach.
//
// Checked before anything is asked of the platform, so a typo is said at
// once rather than after a login.
func TestTunnelNamesAKnownService(t *testing.T) {
	run := tunnelCommand().Run

	for _, args := range [][]string{{}, {"staging", "php"}, {"a", "b", "c"}} {
		if err := run(context.Background(), nil, args); err == nil {
			t.Errorf("%q was accepted", args)
		}
	}

	if err := run(context.Background(), nil, []string{"php"}); err == nil || !strings.Contains(err.Error(), "not a service a tunnel can reach") {
		t.Errorf("an unknown service said: %v", err)
	}
}

// Every service has a usual port to listen on, so a client's defaults work.
func TestEveryTunnelServiceHasAPort(t *testing.T) {
	for _, service := range []string{"db", "mariadb", "postgres", "redis", "valkey", "solr", "meilisearch", "rabbitmq", "memcached", "varnish", "vinyl"} {
		if tunnelPorts[service] == 0 {
			t.Errorf("%s has no default port", service)
		}
	}
}
