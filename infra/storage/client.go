// Package storage uploads email attachments to the Webitel storage service.
package storage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	storagegrpc "buf.build/gen/go/webitel/storage/grpc/go/_gogrpc"
	storagepb "buf.build/gen/go/webitel/storage/protocolbuffers/go"
	kitdiscovery "github.com/webitel/webitel-go-kit/infra/discovery"
	rpc "github.com/webitel/webitel-go-kit/infra/transport/gRPC"
	"github.com/webitel/webitel-go-kit/infra/transport/gRPC/resolver/discovery"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/webitel/webitel-emails/config"
)

const (
	// ServiceName is how storage registers itself in Consul.
	ServiceName = "storage"

	// chunkSize matches the other Webitel uploaders.
	chunkSize = 32 << 10
	// maxFileNameRunes leaves room for the random prefix storage adds itself.
	maxFileNameRunes = 200
	fallbackFileName = "attachment"
)

// UploadRequest is one file to store. Content holds the decoded MIME part.
type UploadRequest struct {
	DomainID int64
	Name     string
	MimeType string
	// ReferenceID identifies the part; storage keeps it as the file reference.
	ReferenceID string
	Content     []byte
}

// UploadResult is what storage reports about the stored file.
type UploadResult struct {
	FileID int64
	Size   int64
}

// Client is the storage FileService client of this service.
type Client struct {
	log           *slog.Logger
	rpc           *rpc.Client[storagegrpc.FileServiceClient]
	uploadTimeout time.Duration
	// uploadSlots bounds the streams open at the same time, and with them the
	// memory held by uploads in flight.
	uploadSlots chan struct{}
}

// NewClient builds the client on service discovery and the kit connection pool.
func NewClient(
	cfg *config.Config,
	log *slog.Logger,
	provider kitdiscovery.DiscoveryProvider,
	lifecycle fx.Lifecycle,
) (*Client, error) {
	factory := func(conn *grpc.ClientConn) storagegrpc.FileServiceClient {
		return storagegrpc.NewFileServiceClient(conn)
	}

	client, err := rpc.NewClient(
		context.Background(),
		factory,
		rpc.WithTarget(fmt.Sprintf("discovery:///%s", ServiceName)),
		rpc.WithDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
			grpc.WithResolvers(discovery.NewBuilder(provider, discovery.WithInsecure(true))),
		),
		rpc.WithRetry(rpc.DefaultRetryConfig()),
		rpc.WithKeepalive(keepalive.ClientParameters{
			Time:                10 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("storage client: create: %w", err)
	}

	storageClient := &Client{
		log:           log,
		rpc:           client,
		uploadTimeout: cfg.Storage.UploadTimeout,
		uploadSlots:   make(chan struct{}, cfg.Storage.MaxConcurrentUploads),
	}
	lifecycle.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return storageClient.Close()
		},
	})

	return storageClient, nil
}

// UploadFile stores one file and returns its storage identity. The stream is not
// retried automatically, so a half-sent upload is a failure for the caller.
func (c *Client) UploadFile(ctx context.Context, req UploadRequest) (UploadResult, error) {
	select {
	case c.uploadSlots <- struct{}{}:
		defer func() { <-c.uploadSlots }()
	case <-ctx.Done():
		return UploadResult{}, ctx.Err()
	}

	uploadCtx, cancel := context.WithTimeout(ctx, c.uploadTimeout)
	defer cancel()

	// GetAPI keeps the connection until cleanup, unlike Execute, which returns it
	// to the pool as soon as its function exits.
	api, cleanup, err := c.rpc.GetAPI(uploadCtx)
	if err != nil {
		return UploadResult{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			c.log.Warn("storage: release connection", "error", cleanupErr)
		}
	}()

	response, err := sendUpload(uploadCtx, api, req)
	if err != nil {
		return UploadResult{}, classifyError(ctx.Err(), err)
	}
	if response.GetCode() == storagepb.UploadStatusCode_Failed || response.GetFileId() <= 0 {
		return UploadResult{}, fmt.Errorf("%w: upload status %s", ErrFileRejected, response.GetCode())
	}

	return UploadResult{FileID: response.GetFileId(), Size: response.GetSize()}, nil
}

// GenerateFileLink returns a short-lived link to a stored file. It is the
// low-level capability the read API builds on.
func (c *Client) GenerateFileLink(
	ctx context.Context,
	req *storagepb.GenerateFileLinkRequest,
) (*storagepb.GenerateFileLinkResponse, error) {
	var response *storagepb.GenerateFileLinkResponse

	err := c.rpc.Execute(ctx, func(api storagegrpc.FileServiceClient) error {
		var callErr error
		response, callErr = api.GenerateFileLink(ctx, req)

		return callErr
	})
	if err != nil {
		return nil, classifyError(ctx.Err(), err)
	}

	return response, nil
}

// Close releases the connection pool.
func (c *Client) Close() error {
	if c.rpc == nil {
		return nil
	}

	return c.rpc.Close()
}

// sendUpload writes the metadata message first and then the file body. An empty
// chunk ends the body on the server, so only non-empty ones are sent.
func sendUpload(
	ctx context.Context,
	api storagegrpc.FileServiceClient,
	req UploadRequest,
) (*storagepb.UploadFileResponse, error) {
	stream, err := api.UploadFile(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: open upload stream: %w", err)
	}

	metadata := &storagepb.UploadFileRequest{
		Data: &storagepb.UploadFileRequest_Metadata_{
			Metadata: &storagepb.UploadFileRequest_Metadata{
				DomainId: req.DomainID,
				Name:     sanitizeFileName(req.Name),
				MimeType: req.MimeType,
				Uuid:     req.ReferenceID,
				Channel:  storagepb.UploadFileChannel_MailChannel,
			},
		},
	}
	if err := stream.Send(metadata); err != nil {
		return nil, fmt.Errorf("storage: send metadata: %w", err)
	}

	for offset := 0; offset < len(req.Content); offset += chunkSize {
		end := min(offset+chunkSize, len(req.Content))
		chunk := &storagepb.UploadFileRequest{
			Data: &storagepb.UploadFileRequest_Chunk{Chunk: req.Content[offset:end]},
		}
		if err := stream.Send(chunk); err != nil {
			return nil, fmt.Errorf("storage: send chunk: %w", err)
		}
	}

	response, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("storage: close upload stream: %w", err)
	}

	return response, nil
}

// sanitizeFileName keeps the name storage stores usable: no path or control
// characters and bounded length. The display name in the manifest is untouched.
func sanitizeFileName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == utf8.RuneError:
			return -1
		case r == '/', r == '\\':
			return '_'
		default:
			return r
		}
	}, name)

	cleaned = strings.Trim(strings.TrimSpace(cleaned), ".")
	if cleaned == "" {
		return fallbackFileName
	}

	if utf8.RuneCountInString(cleaned) > maxFileNameRunes {
		cleaned = string([]rune(cleaned)[:maxFileNameRunes])
	}

	return cleaned
}
