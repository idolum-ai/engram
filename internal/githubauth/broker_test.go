package githubauth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestListenRefusesToReplaceLiveBroker(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "engram-github-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	first, err := Listen(path, func(context.Context, BrokerRequest) BrokerResponse {
		return BrokerResponse{OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := Listen(path, func(context.Context, BrokerRequest) BrokerResponse {
		return BrokerResponse{OK: true}
	})
	if second != nil || err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("second broker = %#v, error = %v", second, err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("live broker socket disappeared: %v", err)
	}
}

func TestBrokerCloseDoesNotRemoveReplacementSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "engram-github-replaced-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	first, err := Listen(path, func(context.Context, BrokerRequest) BrokerResponse {
		return BrokerResponse{OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := Listen(path, func(context.Context, BrokerRequest) BrokerResponse {
		return BrokerResponse{OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("closing displaced broker removed replacement socket: %v", err)
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

func TestTransactionalDeliveryRejectsLegacyProtocolClient(t *testing.T) {
	request := brokerExecTestRequest()
	request.Version = ProtocolVersion - 1
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported GitHub broker protocol version") {
		t.Fatalf("legacy protocol validation error = %v", err)
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

func TestBrokerCommitsTokenOnlyAfterRequesterAcknowledgesDelivery(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "eg-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	finalized := make(chan bool, 1)
	server, err := Listen(path, func(ctx context.Context, _ BrokerRequest) BrokerResponse {
		if err := RegisterDeliveryFinalizer(ctx, func(delivered bool) error {
			finalized <- delivered
			return nil
		}); err != nil {
			return BrokerResponse{Error: err.Error()}
		}
		return BrokerResponse{OK: true, Token: "short-lived-token", ExpiresAt: time.Now().Add(time.Hour)}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	response, err := Request(context.Background(), path, brokerExecTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Token != "short-lived-token" || response.DeliveryPending {
		t.Fatalf("response = %#v", response)
	}
	select {
	case delivered := <-finalized:
		if !delivered {
			t.Fatal("successful requester delivery was rolled back")
		}
	case <-time.After(time.Second):
		t.Fatal("delivery finalizer was not called")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerClearsTokenWhenDeliveryCommitFails(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "eg-commit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	server, err := Listen(path, func(ctx context.Context, _ BrokerRequest) BrokerResponse {
		if err := RegisterDeliveryFinalizer(ctx, func(bool) error {
			return errors.New("binding was revoked before delivery")
		}); err != nil {
			return BrokerResponse{Error: err.Error()}
		}
		return BrokerResponse{OK: true, Token: "must-not-escape"}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	response, err := Request(context.Background(), path, brokerExecTestRequest())
	if err == nil || !strings.Contains(err.Error(), "revoked before delivery") {
		t.Fatalf("delivery error = %v", err)
	}
	if response.Token != "" {
		t.Fatalf("failed delivery exposed token %q", response.Token)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerRollsBackTokenWhenRequesterDisconnectsBeforeCommit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "eg-rollback-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	finalized := make(chan bool, 1)
	server, err := Listen(path, func(ctx context.Context, _ BrokerRequest) BrokerResponse {
		if err := RegisterDeliveryFinalizer(ctx, func(delivered bool) error {
			finalized <- delivered
			return nil
		}); err != nil {
			return BrokerResponse{Error: err.Error()}
		}
		return BrokerResponse{OK: true, Token: "must-be-revoked"}
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
	request := brokerExecTestRequest()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRawBrokerFrame(connection, payload); err != nil {
		t.Fatal(err)
	}
	responsePayload, err := readBrokerFrame(connection)
	if err != nil {
		t.Fatal(err)
	}
	var response BrokerResponse
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		t.Fatal(err)
	}
	if !response.DeliveryPending || response.Token == "" {
		t.Fatalf("pre-commit response = %#v", response)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case delivered := <-finalized:
		if delivered {
			t.Fatal("disconnected requester committed token delivery")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnected delivery was not rolled back")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerShutdownRollsBackClientWithholdingDeliveryAcknowledgment(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "eg-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "github.sock")
	finalized := make(chan bool, 1)
	server, err := Listen(path, func(ctx context.Context, _ BrokerRequest) BrokerResponse {
		if err := RegisterDeliveryFinalizer(ctx, func(delivered bool) error {
			finalized <- delivered
			return nil
		}); err != nil {
			return BrokerResponse{Error: err.Error()}
		}
		return BrokerResponse{OK: true, Token: "provisional-token"}
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
	requestPayload, err := json.Marshal(brokerExecTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRawBrokerFrame(connection, requestPayload); err != nil {
		t.Fatal(err)
	}
	responsePayload, err := readBrokerFrame(connection)
	if err != nil {
		t.Fatal(err)
	}
	var response BrokerResponse
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		t.Fatal(err)
	}
	if !response.DeliveryPending || response.Token == "" {
		t.Fatalf("pre-commit response = %#v", response)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		_ = connection.Close()
		<-done
		t.Fatal("broker shutdown waited for a client withholding delivery acknowledgment")
	}
	select {
	case delivered := <-finalized:
		if delivered {
			t.Fatal("shutdown committed a provisional token")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("shutdown did not roll back the provisional token")
	}
	_ = connection.Close()
}

func brokerExecTestRequest() BrokerRequest {
	return BrokerRequest{
		Version:      ProtocolVersion,
		Action:       ActionExec,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "read"},
		Command:      []string{"gh", "repo", "view"},
		Binding:      Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
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
