package githubauth

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Handler func(context.Context, BrokerRequest) BrokerResponse

type BrokerServer struct {
	path     string
	listener *net.UnixListener
	handler  Handler
	clients  sync.WaitGroup
}

func Listen(path string, handler Handler) (*BrokerServer, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("GitHub broker socket path must be absolute")
	}
	if len(path) > unixSocketPathLimit() {
		return nil, fmt.Errorf("GitHub broker socket path is too long")
	}
	if handler == nil {
		return nil, fmt.Errorf("GitHub broker handler is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create GitHub broker directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	address := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on GitHub broker socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protect GitHub broker socket: %w", err)
	}
	return &BrokerServer{path: path, listener: listener, handler: handler}, nil
}

func (s *BrokerServer) Serve(ctx context.Context) error {
	if s == nil || s.listener == nil {
		return fmt.Errorf("GitHub broker is not listening")
	}
	defer s.clients.Wait()
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		connection, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept GitHub broker request: %w", err)
		}
		s.clients.Add(1)
		go func() {
			defer s.clients.Done()
			s.handleConnection(ctx, connection)
		}()
	}
}

func (s *BrokerServer) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.listener != nil {
		errs = append(errs, s.listener.Close())
	}
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *BrokerServer) handleConnection(serverContext context.Context, connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(4 * time.Minute))
	data, err := readBrokerFrame(connection)
	if err != nil {
		_ = writeBrokerFrame(connection, BrokerResponse{Error: "invalid GitHub broker request"})
		return
	}
	var request BrokerRequest
	if err := json.Unmarshal(data, &request); err != nil {
		_ = writeBrokerFrame(connection, BrokerResponse{Error: "invalid GitHub broker request"})
		return
	}
	request.Normalize()
	if err := request.Validate(); err != nil {
		_ = writeBrokerFrame(connection, BrokerResponse{Error: err.Error()})
		return
	}
	requestContext, cancel := context.WithCancel(serverContext)
	defer cancel()
	peerGone := make(chan struct{})
	go func() {
		var sentinel [1]byte
		_, _ = connection.Read(sentinel[:])
		cancel()
		close(peerGone)
	}()
	response := s.handler(requestContext, request)
	if response.OK {
		response.Error = ""
	} else if response.Error == "" {
		response.Error = "GitHub capability request failed"
	}
	_ = writeBrokerFrame(connection, response)
	_ = connection.Close()
	<-peerGone
}

func Request(ctx context.Context, path string, request BrokerRequest) (BrokerResponse, error) {
	request.Normalize()
	if err := request.Validate(); err != nil {
		return BrokerResponse{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return BrokerResponse{}, fmt.Errorf("encode GitHub broker request: %w", err)
	}
	if len(payload) > MaxRequestBytes {
		return BrokerResponse{}, fmt.Errorf("GitHub broker request exceeds %d bytes", MaxRequestBytes)
	}
	dialer := net.Dialer{}
	raw, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return BrokerResponse{}, fmt.Errorf("connect to Engram GitHub broker: %w", err)
	}
	connection := raw.(*net.UnixConn)
	defer connection.Close()
	requestDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-requestDone:
		}
	}()
	defer close(requestDone)
	deadline := time.Now().Add(4 * time.Minute)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	_ = connection.SetDeadline(deadline)
	if err := writeRawBrokerFrame(connection, payload); err != nil {
		return BrokerResponse{}, fmt.Errorf("send GitHub broker request: %w", err)
	}
	var response BrokerResponse
	responsePayload, err := readBrokerFrame(connection)
	if err != nil {
		return BrokerResponse{}, fmt.Errorf("read GitHub broker response: %w", err)
	}
	if err := json.Unmarshal(responsePayload, &response); err != nil {
		return BrokerResponse{}, fmt.Errorf("decode GitHub broker response: %w", err)
	}
	if !response.OK {
		return response, errors.New(firstNonEmpty(response.Error, "GitHub capability request failed"))
	}
	return response, nil
}

func writeBrokerFrame(writer io.Writer, value BrokerResponse) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRawBrokerFrame(writer, payload)
}

func writeRawBrokerFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxRequestBytes {
		return fmt.Errorf("GitHub broker frame exceeds bounds")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func readBrokerFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxRequestBytes {
		return nil, fmt.Errorf("GitHub broker frame exceeds bounds")
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect GitHub broker socket: %w", err)
	}
	stat, ownerOK := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSocket == 0 || !ownerOK || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("refusing to replace non-private GitHub broker path")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale GitHub broker socket: %w", err)
	}
	return nil
}

func unixSocketPathLimit() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
