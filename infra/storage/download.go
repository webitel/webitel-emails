package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

	storagegrpc "buf.build/gen/go/webitel/storage/grpc/go/_gogrpc"
	storagepb "buf.build/gen/go/webitel/storage/protocolbuffers/go"
)

// ErrFileIncomplete means fewer or more bytes came than the file has; storage may
// end a stream cleanly after a read error.
var ErrFileIncomplete = errors.New("storage: file content is incomplete")

// Open streams a stored file of the domain. The reader fails unless exactly size
// bytes arrive; Close releases the stream at any point.
func (c *Client) Open(ctx context.Context, domainID, fileID, size int64) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	api, cleanup, err := c.rpc.GetAPI(streamCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	stream, err := api.DownloadFile(streamCtx, &storagepb.DownloadFileRequest{Id: fileID, DomainId: domainID})
	if err != nil {
		cancel()
		_ = cleanup()
		return nil, classifyError(ctx.Err(), err)
	}

	return &fileReader{ctx: ctx, stream: stream, left: size, close: func() {
		cancel()
		if err := cleanup(); err != nil {
			c.log.Warn("storage: release connection", "error", err)
		}
	}}, nil
}

type fileReader struct {
	ctx    context.Context
	stream storagegrpc.FileService_DownloadFileClient
	chunk  []byte
	left   int64
	close  func()
	closed bool
}

func (r *fileReader) Read(p []byte) (int, error) {
	for len(r.chunk) == 0 {
		msg, err := r.stream.Recv()
		switch {
		case errors.Is(err, io.EOF) && r.left == 0:
			return 0, io.EOF
		case errors.Is(err, io.EOF):
			return 0, ErrFileIncomplete
		case err != nil:
			return 0, classifyError(r.ctx.Err(), err)
		}
		r.chunk = msg.GetChunk()
	}
	if int64(len(r.chunk)) > r.left {
		return 0, ErrFileIncomplete
	}
	n := copy(p, r.chunk)
	r.chunk, r.left = r.chunk[n:], r.left-int64(n)

	return n, nil
}

func (r *fileReader) Close() error {
	if !r.closed {
		r.closed = true
		r.close()
	}

	return nil
}
