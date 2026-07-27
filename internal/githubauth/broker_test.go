package githubauth

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBrokerRoundTripNeverRequiresTokenOutput(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "engram-github-broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	server, err := Listen(path, func(_ context.Context, request BrokerRequest) BrokerResponse {
		if request.Action != ActionStatus || request.Binding.PaneID != "%3" {
			t.Fatalf("unexpected request: %#v", request)
		}
		return BrokerResponse{OK: true, Leases: []LeaseInfo{{
			App: "idolum", Repositories: []string{"idolum-ai/engram"},
			Permissions: map[string]string{"contents": "read"},
			ExpiresAt:   time.Now().Add(time.Hour),
		}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	response, err := Request(context.Background(), path, BrokerRequest{
		Version: ProtocolVersion,
		Action:  ActionStatus,
		Binding: Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 1 || response.Leases[0].App != "idolum" || response.Token != "" {
		t.Fatalf("unexpected response: %#v", response)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRequestValidationRejectsImplicitAuthority(t *testing.T) {
	request := BrokerRequest{
		Version: ProtocolVersion,
		Action:  ActionExec,
		App:     "idolum",
		Command: []string{"gh"},
		Binding: Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
	}
	if err := request.Validate(); err == nil || err.Error() != "at least one explicit repository is required" {
		t.Fatalf("missing repository error = %v", err)
	}
	request.Repositories = []string{"idolum-ai/engram"}
	if err := request.Validate(); err == nil || err.Error() != "at least one explicit permission is required" {
		t.Fatalf("missing permission error = %v", err)
	}
	request.Permissions = map[string]string{"contents": "admin"}
	if err := request.Validate(); err == nil {
		t.Fatal("unsupported permission level was accepted")
	}
}

func TestBrokerCancelsApprovalWhenRequesterDisconnects(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "engram-github-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	canceled := make(chan struct{})
	server, err := Listen(path, func(ctx context.Context, _ BrokerRequest) BrokerResponse {
		<-ctx.Done()
		close(canceled)
		return BrokerResponse{Error: "canceled"}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	request := BrokerRequest{
		Version:      ProtocolVersion,
		Action:       ActionExec,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "read"},
		Command:      []string{"gh", "repo", "view"},
		Binding:      Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
	}
	payload, _ := json.Marshal(request)
	if err := writeRawBrokerFrame(connection, payload); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request context survived requester disconnect")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompactLeaseLineDistinguishesReadOnlyFromWrite(t *testing.T) {
	now := time.Now().UTC()
	readOnly := LeaseInfo{
		App: "idolum", Repositories: []string{"idolum-ai/engram"},
		Permissions: map[string]string{"contents": "read", "metadata": "read"},
		ExpiresAt:   now.Add(42 * time.Minute),
	}
	if got := CompactLeaseLine(readOnly, now); got != "GH idolum · read-only · 1 repo · 42m" {
		t.Fatalf("read-only line = %q", got)
	}
	readOnly.Permissions["pull_requests"] = "write"
	if got := CompactLeaseLine(readOnly, now); got != "GH idolum · 2R 1W · 1 repo · 42m" {
		t.Fatalf("write line = %q", got)
	}
}
